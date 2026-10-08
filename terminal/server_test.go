package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func testServer(t *testing.T, mutate func(*Config)) (*server, string) {
	t.Helper()
	cfg := Config{
		ListenAddr:      ":0",
		PocketDir:       writePocketConfig(t, "owner-secret-token"),
		Shell:           "sh",
		WorkDir:         t.TempDir(),
		FrameAncestors:  "https://portal.example.com",
		MaxSessions:     4,
		SessionTTLHours: 12,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := newServer(cfg, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func TestHealthzIsPublic(t *testing.T) {
	_, base := testServer(t, nil)
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}

func TestIndexRequiresOwnerToken(t *testing.T) {
	_, base := testServer(t, nil)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET / without token = %d, want 401", resp.StatusCode)
	}
}

func TestIndexTokenSetsCookieAndRedirects(t *testing.T) {
	_, base := testServer(t, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(base + "/?token=owner-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /?token= = %d, want 303", resp.StatusCode)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie set")
	}
	if !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("unsafe session cookie: %+v", session)
	}
	if !strings.HasPrefix(session.Value, "") || len(session.Value) != 64 {
		t.Errorf("session id has unexpected shape")
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.AddCookie(session)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET / with cookie = %d, want 200", resp2.StatusCode)
	}
}

func TestIndexRejectsWrongToken(t *testing.T) {
	_, base := testServer(t, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(base + "/?token=wrong-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /?token=wrong = %d, want 401", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("session cookie set for wrong token")
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, base := testServer(t, nil)
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"wasm-unsafe-eval", "frame-ancestors 'self' https://portal.example.com", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("missing Cache-Control: no-store")
	}
}

func TestStaticAssetsServed(t *testing.T) {
	_, base := testServer(t, nil)
	for path, contentType := range map[string]string{
		"/terminal.js":     "text/javascript",
		"/terminal.css":    "text/css",
		"/ghostty-web.js":  "text/javascript",
		"/ghostty-vt.wasm": "application/wasm",
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, contentType) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, ct, contentType)
		}
	}
}

func dialWS(t *testing.T, base, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := strings.Replace(base, "http", "ws", 1) + "/ws?token=" + token
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func readMessage(t *testing.T, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, data
}

func TestWebSocketRequiresAuth(t *testing.T) {
	_, base := testServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/ws", nil)
	if err == nil {
		t.Error("unauthenticated WebSocket dial succeeded")
	}
}

func TestWebSocketEchoesShell(t *testing.T) {
	s, base := testServer(t, nil)
	s.cfg.Shell = "cat"
	conn := dialWS(t, base, "owner-secret-token")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello-terminal\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		out = append(out, data...)
		if strings.Contains(string(out), "hello-terminal") {
			return
		}
	}
	t.Errorf("shell never echoed input, got %q", out)
	_ = s
}

func TestWebSocketResizeAndExit(t *testing.T) {
	s, base := testServer(t, nil)
	s.cfg.Shell = "sh"
	s.cfg.ShellArgs = []string{"-c", "exit 3"}
	conn := dialWS(t, base, "owner-secret-token")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	for {
		typ, data := readMessage(t, conn)
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.Type != "exit" {
			continue
		}
		if msg.Code != 3 {
			t.Errorf("exit code = %d, want 3", msg.Code)
		}
		return
	}
}

func TestWebSocketCookieSessionEchoesShell(t *testing.T) {
	s, base := testServer(t, nil)
	s.cfg.Shell = "cat"

	jarClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := jarClient.Get(base + "/?token=owner-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	header := http.Header{"Cookie": {session.Name + "=" + session.Value}}
	conn, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial /ws with cookie: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err := conn.Write(ctx, websocket.MessageText, []byte("cookie-shell-ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		out = append(out, data...)
		if strings.Contains(string(out), "cookie-shell-ok") {
			return
		}
	}
	t.Errorf("shell never echoed input, got %q", out)
}
