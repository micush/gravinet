package webadmin

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// logoutCookie runs handleLogout and returns the session cookie it set.
func logoutCookie(t *testing.T, s *Server, r *http.Request) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleLogout(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

// TestLogoutCookieMirrorsLoginCookie is the reported finding. The clearing
// cookie must carry the same attributes as the one it replaces — not because
// an empty value is worth protecting from script, but because "logout clears
// the browser cookie" is the stated reason the revocation list can live in
// memory, and a clearing cookie shaped differently from the cookie it clears
// is a weak thing to rest that on.
func TestLogoutCookieMirrorsLoginCookie(t *testing.T) {
	s := &Server{}
	c := logoutCookie(t, s, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	if c == nil {
		t.Fatal("logout set no session cookie at all")
	}
	if !c.HttpOnly {
		t.Error("logout cookie is not HttpOnly; the login cookie is")
	}
	if !c.Secure {
		t.Error("logout cookie is not Secure; the login cookie is, and webadmin is TLS-only")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("logout cookie SameSite = %v; want Strict, matching the login cookie", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("logout cookie Path = %q; want \"/\" — browsers key deletion on name and path", c.Path)
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("logout cookie is Value=%q MaxAge=%d; want an expiry", c.Value, c.MaxAge)
	}
}

// TestLogoutDoesNotRevokeUnvalidatedTokens is the test for the bug found
// alongside the alert.
//
// /api/logout is deliberately unauthenticated — logging out with a stale
// session has to work. It used to file whatever arrived in the cookie into
// the revocation map, unsigned and unparsed, held for sessionTTL, with the
// opportunistic sweep unable to touch entries that have not expired. That
// made an unauthenticated request into a write on a map that only grows.
func TestLogoutDoesNotRevokeUnvalidatedTokens(t *testing.T) {
	s := &Server{}
	for i := 0; i < 1000; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
		r.AddCookie(&http.Cookie{
			Name:  sessionCookie,
			Value: fmt.Sprintf("junk-%d-%s", i, strings.Repeat("A", 64)),
		})
		s.handleLogout(httptest.NewRecorder(), r)
	}
	if n := len(s.revoked); n != 0 {
		t.Errorf("1000 unauthenticated requests with junk cookies left %d entries in the revocation map; want 0", n)
	}
}

// TestLogoutStillRevokesARealToken is the half that matters more: gating on
// validity must not stop logout from doing its job.
func TestLogoutStillRevokesARealToken(t *testing.T) {
	s := &Server{secret: []byte(strings.Repeat("k", 32))}
	tok := s.newSession("alice")

	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	s.handleLogout(httptest.NewRecorder(), r)

	if _, ok := s.revoked[tok]; !ok {
		t.Fatal("a validly signed token was not revoked on logout")
	}
	// And the revocation must actually take effect.
	r2 := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if user, ok := s.validSession(r2); ok {
		t.Errorf("the revoked token still validates as %q", user)
	}
}

// TestLogoutIgnoresAnExpiredToken records a deliberate consequence of gating
// on validSession rather than on the signature alone: an expired token is not
// filed. That is correct — it is already rejected on its own expiry, and
// recording it would put an entry in the map for a token nothing would
// accept — but it is a behaviour worth pinning rather than rediscovering.
func TestLogoutIgnoresAnExpiredToken(t *testing.T) {
	s := &Server{secret: []byte(strings.Repeat("k", 32))}
	tok := s.newSession("alice")
	// Move the clock past the token's expiry by revoking nothing and simply
	// checking the boundary: build a token that expired an hour ago.
	expired := signedSessionAt(s, "alice", time.Now().Add(-time.Hour).Unix())

	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: expired})
	s.handleLogout(httptest.NewRecorder(), r)
	if _, ok := s.revoked[expired]; ok {
		t.Error("an already-expired token was filed in the revocation map")
	}
	if _, ok := s.revoked[tok]; ok {
		t.Error("an unrelated token was filed")
	}
}

// TestLogoutRequiresPOST covers the cross-site shape. With GET accepted, an
// <img src=".../api/logout"> on any page the operator visited would clear
// their session cookie — the response's clearing cookie is honoured even
// though SameSite=Strict stopped the session cookie being sent, so nothing
// was revoked but the admin was logged out regardless.
func TestLogoutRequiresPOST(t *testing.T) {
	s := &Server{}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		s.handleLogout(w, httptest.NewRequest(m, "/api/logout", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/logout returned %d; want 405", m, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie {
				t.Errorf("%s /api/logout still cleared the session cookie", m)
			}
		}
	}
	// POST must still work.
	w := httptest.NewRecorder()
	s.handleLogout(w, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	if w.Code != http.StatusOK {
		t.Errorf("POST /api/logout returned %d; want 200", w.Code)
	}
}

// signedSessionAt mints a token with an arbitrary expiry, mirroring
// newSession's encoding so a token can be aged without waiting.
func signedSessionAt(s *Server, user string, exp int64) string {
	payload := user + "|" + strconv.FormatInt(exp, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

// TestLogoutRejectsCrossSite closes what v926 left open. Requiring POST
// stopped an <img> tag; a cross-site form post still reached the handler and
// cleared the operator's session cookie, because SameSite governs when a
// cookie is sent, not whether a Set-Cookie is honoured.
func TestLogoutRejectsCrossSite(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site"} {
		s := &Server{}
		r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		s.handleLogout(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %s returned %d; want 403", site, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie {
				t.Errorf("Sec-Fetch-Site: %s still cleared the session cookie", site)
			}
		}
	}
}

// TestLogoutAllowsTheUIAndNonBrowsers is the half that keeps logout working.
// The UI's fetch is same-origin; a script or an older browser sends nothing,
// and must not be locked out of logging out.
func TestLogoutAllowsTheUIAndNonBrowsers(t *testing.T) {
	for _, site := range []string{"same-origin", "none", ""} {
		s := &Server{secret: []byte(strings.Repeat("k", 32))}
		tok := s.newSession("alice")
		r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		s.handleLogout(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site: %q returned %d; want 200", site, w.Code)
		}
		if _, ok := s.revoked[tok]; !ok {
			t.Errorf("Sec-Fetch-Site: %q did not revoke the token", site)
		}
	}
}
