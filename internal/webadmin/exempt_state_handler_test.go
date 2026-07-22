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

// TestExemptEnableDisable covers the per-entry toggle on /api/exempt: the
// op:enable/op:disable path flips one entry by index, and the disabled state
// survives a subsequent whole-list save (the path every UI edit takes), so an
// edit elsewhere doesn't silently re-enable a paused entry.
func TestExemptEnableDisable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Firewall: config.Firewall{Enabled: true}}},
		// Custom allowlist so indices are stable and independent of the defaults.
		FirewallExempts: []config.FirewallExempt{
			{Name: "BGP", Proto: "tcp", Port: 179},
			{Name: "OSPF", Proto: "ospf"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
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
	stored := func() []config.FirewallExempt {
		c2, _ := config.Load(cfgPath)
		return c2.FirewallExempts
	}

	// disable entry 1 (OSPF) via the dedicated op.
	if ok, _ := post(map[string]any{"op": "disable", "index": 1})["ok"].(bool); !ok {
		t.Fatal("disable rejected")
	}
	st := stored()
	if len(st) != 2 || !st[1].Disabled || st[0].Disabled {
		t.Fatalf("only entry 1 should be disabled: %+v", st)
	}

	// A whole-list save (as the UI does for any edit) must preserve the flag.
	save := map[string]any{"exempt": []map[string]any{
		{"name": "BGP", "proto": "tcp", "port": 179},
		{"name": "OSPF", "proto": "ospf", "disabled": true},
	}}
	if ok, _ := post(save)["ok"].(bool); !ok {
		t.Fatal("whole-list save rejected")
	}
	st = stored()
	if len(st) != 2 || !st[1].Disabled {
		t.Fatalf("disabled flag should survive a whole-list save: %+v", st)
	}

	// re-enable entry 1 via the op.
	if ok, _ := post(map[string]any{"op": "enable", "index": 1})["ok"].(bool); !ok {
		t.Fatal("enable rejected")
	}
	if stored()[1].Disabled {
		t.Fatalf("entry 1 should be enabled again: %+v", stored())
	}

	// out-of-range index is rejected.
	if ok, _ := post(map[string]any{"op": "disable", "index": 9})["ok"].(bool); ok {
		t.Error("out-of-range index should be rejected")
	}
}
