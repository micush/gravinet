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

// TestHandleNetworkAddress covers the overlay-address edit on /api/network: the
// address op persists this node's own overlay address and reports restart:true
// (a running interface isn't re-addressed live), and validation is enforced.
func TestHandleNetworkAddress(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/16"}},
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/network", bytes.NewReader(b))
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
	stored := func() config.Network {
		c2, _ := config.Load(cfgPath)
		return c2.Networks[0]
	}

	// set the overlay address; expect ok and restart:true.
	res := post(map[string]any{"op": "address", "net": "1234", "address4": "10.0.0.42/16"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatal("address set rejected")
	}
	if restart, _ := res["restart"].(bool); !restart {
		t.Error("address change should report restart required")
	}
	if stored().Address4 != "10.0.0.42/16" {
		t.Fatalf("address not persisted: %+v", stored())
	}

	// clear it with "none".
	if ok, _ := post(map[string]any{"op": "address", "net": "1234", "address4": "none"})["ok"].(bool); !ok {
		t.Fatal("address clear rejected")
	}
	if stored().Address4 != "" {
		t.Fatalf("address not cleared: %+v", stored())
	}

	// out-of-subnet address is rejected.
	if ok, _ := post(map[string]any{"op": "address", "net": "1234", "address4": "192.168.1.5/24"})["ok"].(bool); ok {
		t.Error("out-of-subnet address should be rejected")
	}
}
