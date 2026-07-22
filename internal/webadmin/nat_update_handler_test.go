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

// TestHandleNATRuleUpdate covers the click-to-edit path on /api/nat: the update
// op replaces a rule's fields in place by index, preserving its enabled state
// and position, and shares NATRuleAdd's validation.
func TestHandleNATRuleUpdate(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			NAT: config.NAT{Enabled: true, Rules: []config.NATRule{
				{Source: "10.0.0.0/24", Translate: "masquerade", Interface: "eth0", Enabled: false},
				{Source: "10.0.1.0/24", Translate: "198.51.100.7", Enabled: true},
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
	rule0 := func() config.NATRule {
		c2, _ := config.Load(cfgPath)
		return c2.Networks[0].NAT.Rules[0]
	}

	// edit rule 0: masquerade -> port-forward (DNAT), new dest. State
	// (disabled) and position preserved. There's no separate "direction" key
	// to post anymore — port-forward: is part of translate itself.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "index": 0,
		"source": "10.0.0.0/24", "dest": "203.0.113.0/24", "translate": "port-forward:192.0.2.5"})["ok"].(bool); !ok {
		t.Fatal("update rejected")
	}
	r := rule0()
	if r.Translate != "port-forward:192.0.2.5" || r.Interface != "" || r.Dest != "203.0.113.0/24" {
		t.Fatalf("fields not updated correctly: %+v", r)
	}
	if r.Enabled {
		t.Fatal("update must preserve the disabled state")
	}
	c2, _ := config.Load(cfgPath)
	if len(c2.Networks[0].NAT.Rules) != 2 || c2.Networks[0].NAT.Rules[1].Source != "10.0.1.0/24" {
		t.Fatalf("update must not reorder/drop rules: %+v", c2.Networks[0].NAT.Rules)
	}

	// masquerade without an interface is rejected.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "index": 0, "translate": "masquerade"})["ok"].(bool); ok {
		t.Error("masquerade without iface should be rejected")
	}
	// out-of-range index rejected.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "index": 9, "translate": "192.0.2.9"})["ok"].(bool); ok {
		t.Error("out-of-range index should be rejected")
	}
}
