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

// TestExemptHandler verifies the global /api/exempt endpoint: GET reports the
// list (resolving the management port to a number), POST replaces or resets it,
// persists to config, and reports restart:false (applied live via reload).
func TestExemptHandler(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Firewall: config.Firewall{Enabled: true}}},
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

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/exempt", bytes.NewReader(b))
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
	get := func() map[string]any {
		req, _ := http.NewRequest("GET", ts.URL+"/api/exempt", nil)
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
	stored := func() []config.FirewallExempt {
		c2, _ := config.Load(cfgPath)
		return c2.FirewallExempts
	}

	// GET on a fresh config returns the defaults, flagged default, with the
	// management entry resolved to the actual web-admin port number.
	g := get()
	if d, _ := g["default"].(bool); !d {
		t.Error("fresh config should report default=true")
	}
	if mp, _ := g["mgmt_port"].(float64); int(mp) != 8443 {
		t.Errorf("mgmt_port = %v, want 8443", g["mgmt_port"])
	}
	list, _ := g["exempt"].([]any)
	var sawMgmtNumber bool
	for _, item := range list {
		e := item.(map[string]any)
		if e["name"] == "remote management" {
			if p, _ := e["port"].(float64); int(p) == 8443 {
				sawMgmtNumber = true
			}
		}
	}
	if !sawMgmtNumber {
		t.Error("remote management entry should report the actual port number 8443")
	}

	// POST replaces the whole list (applied live), preserving a mgmt entry's flag.
	out := post(map[string]any{"exempt": []map[string]any{
		{"name": "remote management", "proto": "tcp", "mgmt": true},
		{"name": "vxlan", "proto": "udp", "port": 4789},
	}})
	if out["error"] != nil {
		t.Fatalf("set errored: %v", out["error"])
	}
	if r, _ := out["restart"].(bool); r {
		t.Fatal("exempt edits apply live; restart must be false")
	}
	ex := stored()
	if len(ex) != 2 {
		t.Fatalf("after set, stored exempts = %d, want 2", len(ex))
	}
	if !ex[0].Mgmt {
		t.Error("mgmt flag should be preserved on the management entry")
	}
	if ex[1].Name != "vxlan" || ex[1].Port != 4789 {
		t.Errorf("custom entry not persisted: %+v", ex[1])
	}

	// Invalid proto in a set is rejected and leaves the list unchanged.
	out = post(map[string]any{"exempt": []map[string]any{{"name": "bad", "proto": "frob"}}})
	if out["error"] == nil {
		t.Fatal("invalid proto should be rejected")
	}
	if len(stored()) != 2 {
		t.Error("rejected set must not mutate the stored list")
	}

	// Reset reverts to defaults (nil in config => omitted).
	out = post(map[string]any{"reset": true})
	if out["error"] != nil {
		t.Fatalf("reset errored: %v", out["error"])
	}
	if ex := stored(); ex != nil {
		t.Fatalf("after reset, stored exempts should be nil (defaults), got %+v", ex)
	}
}
