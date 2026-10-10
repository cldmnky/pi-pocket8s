package githubauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRecentLoginRequirement proves that holding a valid session is not the
// same as having signed in recently: only a fresh sign-in satisfies an
// operation that demands one, and the requirement never refreshes itself.
func TestRecentLoginRequirement(t *testing.T) {
	p := &provider{}
	a := New(p, "https://portal.example")
	now := time.Now()
	a.now = func() time.Time { return now }
	cookie := login(t, a)
	req := httptest.NewRequest("GET", "https://portal.example/api/admin/activate", nil)
	req.AddCookie(cookie)

	if _, err := a.AuthorizeRecent(req, 15*time.Minute); err != nil {
		t.Fatalf("fresh sign-in rejected: %v", err)
	}
	// The session stays valid for hours, but the sign-in stops being recent.
	now = now.Add(16 * time.Minute)
	if _, err := a.Authorize(req); err != nil {
		t.Fatalf("session should still be valid: %v", err)
	}
	if _, err := a.AuthorizeRecent(req, 15*time.Minute); err != ErrStaleLogin {
		t.Fatalf("stale sign-in accepted: %v", err)
	}
	// A membership re-check must not refresh the sign-in time.
	now = now.Add(6 * time.Minute)
	if _, err := a.Authorize(req); err != nil {
		t.Fatalf("re-check failed: %v", err)
	}
	if _, err := a.AuthorizeRecent(req, 15*time.Minute); err != ErrStaleLogin {
		t.Fatalf("re-check refreshed the sign-in time: %v", err)
	}
	// Signing in again makes it recent once more.
	cookie = login(t, a)
	req = httptest.NewRequest("GET", "https://portal.example/api/admin/activate", nil)
	req.AddCookie(cookie)
	if _, err := a.AuthorizeRecent(req, 15*time.Minute); err != nil {
		t.Fatalf("new sign-in rejected: %v", err)
	}
	// A request without a session is unauthorized, not merely stale.
	if _, err := a.AuthorizeRecent(httptest.NewRequest("GET", "https://portal.example/x", nil), time.Minute); err != ErrUnauthorized {
		t.Fatalf("missing session: %v", err)
	}
	// A non-positive window can never be satisfied by an old session.
	now = now.Add(time.Hour)
	if _, err := a.AuthorizeRecent(req, 0); err != ErrStaleLogin {
		t.Fatalf("zero window accepted: %v", err)
	}
	// And an expired session is unauthorized regardless of recency.
	now = now.Add(9 * time.Hour)
	if _, err := a.AuthorizeRecent(req, time.Hour); err != ErrUnauthorized {
		t.Fatalf("expired session: %v", err)
	}
}

var _ = http.StatusOK
