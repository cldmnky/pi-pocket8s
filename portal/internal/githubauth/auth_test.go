package githubauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
)

type provider struct {
	deny   bool
	checks int
}

func (*provider) AuthorizationURL(callback, state, verifier string) string {
	return "https://github.com/login/oauth/authorize?state=" + url.QueryEscape(state)
}
func (p *provider) Login(context.Context, string, string, string) (githubapp.User, error) {
	if p.deny {
		return githubapp.User{}, errors.New("denied")
	}
	return githubapp.User{ID: 42, Login: "alice"}, nil
}
func (p *provider) CheckMember(context.Context, string) error {
	p.checks++
	if p.deny {
		return errors.New("denied")
	}
	return nil
}

func login(t *testing.T, a *Auth) *http.Cookie {
	t.Helper()
	start := httptest.NewRecorder()
	a.Start(start, httptest.NewRequest("GET", "https://portal.example/auth/github/start", nil))
	cookies := start.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("OAuth cookie missing")
	}
	cookie := cookies[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Error("unsafe OAuth cookie")
	}
	target, _ := url.Parse(start.Header().Get("Location"))
	state := target.Query().Get("state")
	callback := httptest.NewRequest("GET", "https://portal.example/auth/github/callback?code=code&state="+state, nil)
	callback.AddCookie(cookie)
	response := httptest.NewRecorder()
	a.Callback(response, callback)
	if response.Code != 303 {
		t.Fatalf("callback %d: %s", response.Code, response.Body)
	}
	for _, c := range response.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatal("session cookie missing")
	return nil
}

func TestBrowserBindingReplaySessionAndRevalidation(t *testing.T) {
	p := &provider{}
	a := New(p, "https://portal.example")
	now := time.Now()
	a.now = func() time.Time { return now }
	start := httptest.NewRecorder()
	a.Start(start, httptest.NewRequest("GET", "https://portal.example/auth/github/start", nil))
	target, _ := url.Parse(start.Header().Get("Location"))
	withoutCookie := httptest.NewRecorder()
	a.Callback(withoutCookie, httptest.NewRequest("GET", "https://portal.example/auth/github/callback?code=code&state="+target.Query().Get("state"), nil))
	if withoutCookie.Code != 400 {
		t.Error("callback not bound to browser")
	}
	cookie := login(t, a)
	if !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.MaxAge != 28800 {
		t.Error("unsafe session cookie")
	}
	req := httptest.NewRequest("GET", "https://portal.example/api/config", nil)
	req.AddCookie(cookie)
	if user, err := a.Authorize(req); err != nil || user.Login != "alice" {
		t.Fatalf("session %v %v", user, err)
	}
	if _, err := a.Authorize(httptest.NewRequest("GET", "https://portal.example/api/config", nil)); err == nil {
		t.Error("missing cookie authorized")
	}
	now = now.Add(6 * time.Minute)
	p.deny = true
	if _, err := a.Authorize(req); err == nil {
		t.Error("removed/unavailable membership authorized")
	}
	p.deny = false
	if _, err := a.Authorize(req); err != nil || p.checks != 2 {
		t.Error("membership not rechecked")
	}
	forgedLogout := httptest.NewRecorder()
	a.Logout(forgedLogout, req)
	if forgedLogout.Code != 403 {
		t.Error("missing Origin logout allowed")
	}
	req.Header.Set("Origin", "https://portal.example")
	logout := httptest.NewRecorder()
	a.Logout(logout, req)
	if logout.Code != 204 {
		t.Error("logout failed")
	}
	if _, err := a.Authorize(req); err == nil {
		t.Error("logged out session accepted")
	}
	cookie = login(t, a)
	req = httptest.NewRequest("GET", "https://portal.example/api/config", nil)
	req.AddCookie(cookie)
	now = now.Add(9 * time.Hour)
	if _, err := a.Authorize(req); err == nil {
		t.Error("expired session accepted")
	}
}

func TestInvalidStateSingleUseAndDeniedLogin(t *testing.T) {
	p := &provider{}
	a := New(p, "https://portal.example")
	for _, query := range []string{"code=code&state=wrong", "state=", "code=code&state=valid&state=other"} {
		response := httptest.NewRecorder()
		a.Start(response, httptest.NewRequest("GET", "https://portal.example/auth/github/start", nil))
		req := httptest.NewRequest("GET", "https://portal.example/auth/github/callback?"+query, nil)
		req.AddCookie(response.Result().Cookies()[0])
		callback := httptest.NewRecorder()
		a.Callback(callback, req)
		if callback.Code != 400 {
			t.Error("invalid state accepted")
		}
		callback = httptest.NewRecorder()
		a.Callback(callback, req)
		if callback.Code != 400 {
			t.Error("replay accepted")
		}
	}
	p.deny = true
	response := httptest.NewRecorder()
	a.Start(response, httptest.NewRequest("GET", "https://portal.example/auth/github/start", nil))
	target, _ := url.Parse(response.Header().Get("Location"))
	req := httptest.NewRequest("GET", "https://portal.example/auth/github/callback?code=code&state="+target.Query().Get("state"), nil)
	req.AddCookie(response.Result().Cookies()[0])
	callback := httptest.NewRecorder()
	a.Callback(callback, req)
	if callback.Code != 403 {
		t.Error("unauthorized member signed in")
	}
}
