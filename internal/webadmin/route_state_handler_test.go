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

// TestHandleRouteEnableDisable covers the per-entry toggle ops on the route
// endpoint: enable/disable for an advertised route and reject-enable/
// reject-disable for a reject entry, each keyed by CIDR and applied live.
func TestHandleRouteEnableDisable(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Routes:   []config.Route{{CIDR: "192.168.5.0/24", Enabled: true}},
			RouteRej: []config.RejectRoute{{CIDR: "10.0.0.0/8"}},
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

	// disable the advertised route.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "cidr": "192.168.5.0/24"})["ok"].(bool); !ok {
		t.Fatal("route disable rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].Routes) != 1 || got.Networks[0].Routes[0].Enabled {
		t.Fatalf("route should be present and disabled: %+v", got.Networks[0].Routes)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "enable", "net": "lan", "cidr": "192.168.5.0/24"})["ok"].(bool); !ok {
		t.Fatal("route enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if !got.Networks[0].Routes[0].Enabled {
		t.Fatalf("route should be enabled again: %+v", got.Networks[0].Routes)
	}

	// disable the reject entry.
	if ok, _ := post(map[string]any{"op": "reject-disable", "net": "lan", "cidr": "10.0.0.0/8"})["ok"].(bool); !ok {
		t.Fatal("reject-disable rejected")
	}
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].RouteRej) != 1 || !got.Networks[0].RouteRej[0].Disabled {
		t.Fatalf("reject entry should be present and disabled: %+v", got.Networks[0].RouteRej)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "reject-enable", "net": "lan", "cidr": "10.0.0.0/8"})["ok"].(bool); !ok {
		t.Fatal("reject-enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.Networks[0].RouteRej[0].Disabled {
		t.Fatalf("reject entry should be enabled again: %+v", got.Networks[0].RouteRej)
	}

	// toggling missing entries is rejected.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "cidr": "203.0.113.0/24"})["ok"].(bool); ok {
		t.Error("disabling a missing route should be rejected")
	}
	if ok, _ := post(map[string]any{"op": "reject-disable", "net": "lan", "cidr": "203.0.113.0/24"})["ok"].(bool); ok {
		t.Error("disabling a missing reject entry should be rejected")
	}
}
