package githubauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cldmnky/pi-pocket8s/portal/internal/githubapp"
)

type failingProvider struct{ provider }

func (*failingProvider) Login(context.Context, string, string, string) (githubapp.User, error) {
	return githubapp.User{}, errors.New("secret-provider-detail")
}

func TestOAuthLogsStagesWithoutCredentials(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	a := New(&provider{}, "https://portal.example", logger)
	cookie := login(t, a)
	if !strings.Contains(logs.String(), "outcome=authenticated") {
		t.Fatal("missing success event")
	}
	if strings.Contains(logs.String(), cookie.Value) {
		t.Fatal("session token logged")
	}

	a = New(&failingProvider{}, "https://portal.example", logger)
	start := httptest.NewRecorder()
	a.Start(start, httptest.NewRequest("GET", "https://portal.example/auth/github/start", nil))
	target, _ := url.Parse(start.Header().Get("Location"))
	state := target.Query().Get("state")
	req := httptest.NewRequest("GET", "https://portal.example/auth/github/callback?code=secret-code&state="+state, nil)
	transaction := start.Result().Cookies()[0]
	req.AddCookie(transaction)
	callback := httptest.NewRecorder()
	a.Callback(callback, req)
	if callback.Code != 403 {
		t.Fatalf("status %d", callback.Code)
	}
	if !strings.Contains(logs.String(), "stage=identity_and_membership outcome=failed reason=provider_unavailable") {
		t.Fatal("missing safe failure event")
	}
	for _, secret := range []string{"secret-code", "secret-provider-detail", state, transaction.Value} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("OAuth secret/detail logged")
		}
	}
	a.Callback(httptest.NewRecorder(), httptest.NewRequest("GET", "https://portal.example/auth/github/callback", nil))
	if !strings.Contains(logs.String(), "reason=missing_cookie") {
		t.Fatal("missing transaction failure event")
	}
}
