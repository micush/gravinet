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

func TestHandleHostAddRemove(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Firewall: config.Firewall{Enabled: true}}},
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
	reloads := 0
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { reloads++; return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/host", bytes.NewReader(b))
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

	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "name": "web.local", "ip": "192.168.5.5"})["ok"].(bool); !ok {
		t.Fatal("add rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].HostsAdvertise) != 1 || got.Networks[0].HostsAdvertise[0].IP != "192.168.5.5" {
		t.Fatalf("record not saved: %+v", got.Networks[0].HostsAdvertise)
	}
	if got.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("a freshly added record should be enabled")
	}
	if reloads == 0 {
		t.Error("reload not triggered")
	}
	// disable the record — kept in config, flag flipped.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "name": "web.local"})["ok"].(bool); !ok {
		t.Fatal("disable rejected")
	}
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].HostsAdvertise) != 1 || !got.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatalf("record should be present and disabled: %+v", got.Networks[0].HostsAdvertise)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "enable", "net": "lan", "name": "web.local"})["ok"].(bool); !ok {
		t.Fatal("enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatalf("record should be enabled again: %+v", got.Networks[0].HostsAdvertise)
	}
	// toggling a missing record is rejected.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "name": "nope"})["ok"].(bool); ok {
		t.Error("disabling a missing record should be rejected")
	}
	// invalid IP rejected
	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "name": "bad", "ip": "nope"})["ok"].(bool); ok {
		t.Error("invalid IP should be rejected")
	}
	// remove
	post(map[string]any{"op": "remove", "net": "lan", "name": "web.local"})
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].HostsAdvertise) != 0 {
		t.Errorf("record not removed: %+v", got.Networks[0].HostsAdvertise)
	}
}
