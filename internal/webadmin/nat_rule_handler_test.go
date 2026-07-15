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

// TestHandleNATRuleEnableDisable covers the per-rule enable/disable ops on the
// NAT endpoint: a rule is toggled by its index, kept in config with its match
// intact, and the change applies live. Mirrors the firewall per-rule toggle.
func TestHandleNATRuleEnableDisable(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			NAT: config.NAT{Enabled: true, Rules: []config.NATRule{
				{Direction: config.NATOverlayToUnderlay, Source: "10.0.0.0/24", Translate: "198.51.100.7", Enabled: true},
			}},
		}},
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/nat", bytes.NewReader(b))
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

	// disable rule 0.
	if ok, _ := post(map[string]any{"op": "rule-disable", "net": "lan", "index": 0})["ok"].(bool); !ok {
		t.Fatal("rule-disable rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].NAT.Rules) != 1 || got.Networks[0].NAT.Rules[0].Enabled {
		t.Fatalf("rule should be present and disabled: %+v", got.Networks[0].NAT.Rules)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "rule-enable", "net": "lan", "index": 0})["ok"].(bool); !ok {
		t.Fatal("rule-enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if !got.Networks[0].NAT.Rules[0].Enabled {
		t.Fatalf("rule should be enabled again: %+v", got.Networks[0].NAT.Rules)
	}
	// out-of-range index is rejected.
	if ok, _ := post(map[string]any{"op": "rule-disable", "net": "lan", "index": 9})["ok"].(bool); ok {
		t.Error("out-of-range index should be rejected")
	}
}
