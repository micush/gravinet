package webadmin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// TestIndexNoStore verifies the app shell (HTML + embedded JS) is served with
// Cache-Control: no-store — the same staleness guard authed() already applies
// to every /api/* call (see its own comment), but which handleIndex lacked
// since "/" is registered directly and never passes through authed(). Without
// this, a browser could keep running JavaScript from before a server upgrade
// indefinitely: every /api/* call it makes still reaches the current,
// upgraded backend and "succeeds" by the old JS's own (stale) logic, so
// nothing about the requests themselves would look wrong.
func TestIndexNoStore(t *testing.T) {
	cred, _ := GenerateCredential("admin", "pw", 10000)
	cfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(cfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(t.TempDir() + "/config.json")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
