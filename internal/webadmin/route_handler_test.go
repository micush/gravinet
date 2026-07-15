package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// TestRouteAppliesLive verifies /api/route reports restart:false (redistributing
// or removing a route now applies live via the reload) and persists the change.
func TestRouteAppliesLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(op, cidr string) map[string]any {
		b, _ := json.Marshal(map[string]any{"net": "1234", "op": op, "cidr": cidr})
		req, _ := http.NewRequest("POST", ts.URL+"/api/route", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	routes := func() []config.Route {
		c2, _ := config.Load(cfgPath)
		return c2.Networks[0].Routes
	}

	out := post("redistribute", "192.168.99.0/24")
	if out["error"] != nil {
		t.Fatalf("redistribute errored: %v", out["error"])
	}
	if r, _ := out["restart"].(bool); r {
		t.Fatal("redistributing a route applies live; restart must be false")
	}
	if rs := routes(); len(rs) != 1 || rs[0].CIDR != "192.168.99.0/24" {
		t.Fatalf("config routes = %+v, want one 192.168.99.0/24", rs)
	}

	out = post("delete", "192.168.99.0/24")
	if r, _ := out["restart"].(bool); r {
		t.Fatal("deleting a route applies live; restart must be false")
	}
	if rs := routes(); len(rs) != 0 {
		t.Fatalf("config routes = %+v, want empty after delete", rs)
	}
}
