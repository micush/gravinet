package webadmin

import (
	"net/http"
	"path/filepath"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

func newServerWithPath(t *testing.T, dir string) *Server {
	t.Helper()
	cred, _ := GenerateCredential("admin", "pw", 10000)
	cfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred}, LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	s := New(cfg, &stubBackend{}, logx.Default())
	s.SetConfigPath(filepath.Join(dir, "config.json"))
	return s
}

// TestSessionSurvivesRestart: a cookie issued by one server validates on a fresh
// server that shares the same persisted signing key (simulating a daemon restart).
func TestSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := newServerWithPath(t, dir)
	tok := s1.newSession("admin")

	// New process: a brand-new Server pointed at the same config dir loads the
	// persisted key from disk.
	s2 := newServerWithPath(t, dir)
	r, _ := http.NewRequest("GET", "/api/config", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	user, ok := s2.validSession(r)
	if !ok || user != "admin" {
		t.Fatalf("session should survive restart: ok=%v user=%q", ok, user)
	}
}

// TestSessionRejectsForgedKey: a token signed under a different key is rejected.
func TestSessionRejectsForgedKey(t *testing.T) {
	s1 := newServerWithPath(t, t.TempDir())
	tok := s1.newSession("admin")
	s2 := newServerWithPath(t, t.TempDir()) // different dir => different key
	r, _ := http.NewRequest("GET", "/api/config", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if _, ok := s2.validSession(r); ok {
		t.Fatal("token signed under a different key must be rejected")
	}
}

// TestSessionRejectsTamper: flipping the payload invalidates the signature.
func TestSessionRejectsTamper(t *testing.T) {
	s := newServerWithPath(t, t.TempDir())
	tok := s.newSession("admin")
	// Corrupt a byte in the payload section.
	bad := []byte(tok)
	bad[0] ^= 0xff
	r, _ := http.NewRequest("GET", "/api/config", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: string(bad)})
	if _, ok := s.validSession(r); ok {
		t.Fatal("tampered token must be rejected")
	}
}
