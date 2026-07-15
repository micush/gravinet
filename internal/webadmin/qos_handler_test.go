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

// TestHandleQoSRuleEnableDisable covers the per-rule enable/disable ops on the
// QoS endpoint: a disabled rule is kept in config with its match intact and the
// flag flipped, and re-enabling clears it. Mirrors the firewall per-rule toggle.
func TestHandleQoSRuleEnableDisable(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/qos", bytes.NewReader(b))
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

	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "proto": "tcp", "port": 22, "class": 0})["ok"].(bool); !ok {
		t.Fatal("add rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].QoS.Rules) != 1 || got.Networks[0].QoS.Rules[0].Disabled {
		t.Fatalf("rule should be present and enabled: %+v", got.Networks[0].QoS.Rules)
	}
	// disable the rule.
	if ok, _ := post(map[string]any{"op": "rule-disable", "net": "lan", "proto": "tcp", "port": 22})["ok"].(bool); !ok {
		t.Fatal("rule-disable rejected")
	}
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].QoS.Rules) != 1 || !got.Networks[0].QoS.Rules[0].Disabled {
		t.Fatalf("rule should be present and disabled: %+v", got.Networks[0].QoS.Rules)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "rule-enable", "net": "lan", "proto": "tcp", "port": 22})["ok"].(bool); !ok {
		t.Fatal("rule-enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.Networks[0].QoS.Rules[0].Disabled {
		t.Fatalf("rule should be enabled again: %+v", got.Networks[0].QoS.Rules)
	}
	// toggling a missing rule is rejected.
	if ok, _ := post(map[string]any{"op": "rule-disable", "net": "lan", "proto": "udp", "port": 9999})["ok"].(bool); ok {
		t.Error("disabling a missing rule should be rejected")
	}
}
