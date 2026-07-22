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

// TestHandleHostUpdate covers the click-to-edit path on /api/host: the update op
// renames a record and/or changes its IP in place, preserving its disabled state
// and position, and rejects a rename onto an existing record.
func TestHandleHostUpdate(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			HostsAdvertise: []config.HostRecord{
				{Name: "web.local", IP: "192.168.5.5", Disabled: true},
				{Name: "db.local", IP: "192.168.5.6"},
			},
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
	rec0 := func() config.HostRecord {
		c2, _ := config.Load(cfgPath)
		return c2.Networks[0].HostsAdvertise[0]
	}

	// edit IP only — disabled state and name preserved.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "name": "web.local", "newname": "web.local", "ip": "10.0.0.9"})["ok"].(bool); !ok {
		t.Fatal("ip update rejected")
	}
	if r := rec0(); r.IP != "10.0.0.9" || r.Name != "web.local" || !r.Disabled {
		t.Fatalf("ip edit should preserve name+disabled: %+v", r)
	}

	// rename — position (index 0) and disabled state preserved.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "name": "web.local", "newname": "www.local", "ip": "10.0.0.9"})["ok"].(bool); !ok {
		t.Fatal("rename rejected")
	}
	if r := rec0(); r.Name != "www.local" || !r.Disabled {
		t.Fatalf("rename should preserve position+disabled: %+v", r)
	}

	// rename onto an existing record is rejected.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "name": "www.local", "newname": "db.local", "ip": "10.0.0.9"})["ok"].(bool); ok {
		t.Error("renaming onto an existing record should be rejected")
	}
	// invalid IP is rejected.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "name": "www.local", "newname": "www.local", "ip": "nope"})["ok"].(bool); ok {
		t.Error("invalid ip should be rejected")
	}
}
