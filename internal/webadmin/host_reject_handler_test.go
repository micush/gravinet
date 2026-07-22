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

// TestHandleHostReject covers the reject ops on /api/host: add / reject-disable /
// reject-enable / reject-remove for the local refuse-list, persisted to config.
func TestHandleHostReject(t *testing.T) {
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
	rejects := func() []config.HostReject {
		c2, _ := config.Load(cfgPath)
		return c2.Networks[0].HostsReject
	}

	// add a reject.
	if ok, _ := post(map[string]any{"op": "reject", "net": "lan", "name": "bad.local"})["ok"].(bool); !ok {
		t.Fatal("reject add rejected")
	}
	if r := rejects(); len(r) != 1 || r[0].Name != "bad.local" || r[0].Disabled {
		t.Fatalf("reject not persisted: %+v", r)
	}
	// disable it.
	if ok, _ := post(map[string]any{"op": "reject-disable", "net": "lan", "name": "bad.local"})["ok"].(bool); !ok {
		t.Fatal("reject-disable rejected")
	}
	if r := rejects(); len(r) != 1 || !r[0].Disabled {
		t.Fatalf("reject should be disabled: %+v", r)
	}
	// re-enable it.
	if ok, _ := post(map[string]any{"op": "reject-enable", "net": "lan", "name": "bad.local"})["ok"].(bool); !ok {
		t.Fatal("reject-enable rejected")
	}
	if r := rejects(); r[0].Disabled {
		t.Fatalf("reject should be enabled again: %+v", r)
	}
	// remove it.
	if ok, _ := post(map[string]any{"op": "reject-remove", "net": "lan", "name": "bad.local"})["ok"].(bool); !ok {
		t.Fatal("reject-remove rejected")
	}
	if r := rejects(); len(r) != 0 {
		t.Fatalf("reject not removed: %+v", r)
	}
	// removing a missing entry is rejected.
	if ok, _ := post(map[string]any{"op": "reject-remove", "net": "lan", "name": "nope"})["ok"].(bool); ok {
		t.Error("removing a missing reject should be rejected")
	}
}
