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

func TestHandleDNSAddUpdateRemove(t *testing.T) {
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/dns", bytes.NewReader(b))
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

	// add, with a multi-server, comma-separated (and whitespace-padded) list.
	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "domain": "corp.internal", "servers": "1.1.1.1, 2.2.2.2"})["ok"].(bool); !ok {
		t.Fatal("add rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].DNSAdvertise) != 1 {
		t.Fatalf("record not saved: %+v", got.Networks[0].DNSAdvertise)
	}
	fwd := got.Networks[0].DNSAdvertise[0]
	if fwd.Domain != "corp.internal" || len(fwd.Servers) != 2 || fwd.Servers[0] != "1.1.1.1" || fwd.Servers[1] != "2.2.2.2" {
		t.Fatalf("server list not parsed correctly: %+v", fwd)
	}
	if fwd.Disabled {
		t.Fatal("a freshly added forward should be enabled")
	}
	if reloads == 0 {
		t.Error("reload not triggered")
	}

	// disable — kept in config, flag flipped, servers untouched.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "domain": "corp.internal"})["ok"].(bool); !ok {
		t.Fatal("disable rejected")
	}
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].DNSAdvertise) != 1 || !got.Networks[0].DNSAdvertise[0].Disabled {
		t.Fatalf("forward should be present and disabled: %+v", got.Networks[0].DNSAdvertise)
	}

	// re-enable.
	if ok, _ := post(map[string]any{"op": "enable", "net": "lan", "domain": "corp.internal"})["ok"].(bool); !ok {
		t.Fatal("enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.Networks[0].DNSAdvertise[0].Disabled {
		t.Fatalf("forward should be enabled again: %+v", got.Networks[0].DNSAdvertise)
	}

	// update: rename the domain and change the server list in one call.
	if ok, _ := post(map[string]any{"op": "update", "net": "lan", "domain": "corp.internal", "newdomain": "eng.internal", "servers": "9.9.9.9"})["ok"].(bool); !ok {
		t.Fatal("update rejected")
	}
	got, _ = config.Load(cfgPath)
	fwd = got.Networks[0].DNSAdvertise[0]
	if fwd.Domain != "eng.internal" || len(fwd.Servers) != 1 || fwd.Servers[0] != "9.9.9.9" {
		t.Fatalf("update did not apply: %+v", fwd)
	}

	// toggling a missing forward is rejected.
	if ok, _ := post(map[string]any{"op": "disable", "net": "lan", "domain": "nope.internal"})["ok"].(bool); ok {
		t.Error("disabling a missing forward should be rejected")
	}
	// invalid server rejected.
	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "domain": "bad.internal", "servers": "not-an-ip"})["ok"].(bool); ok {
		t.Error("invalid server should be rejected")
	}
	// empty server list rejected.
	if ok, _ := post(map[string]any{"op": "add", "net": "lan", "domain": "empty.internal", "servers": ""})["ok"].(bool); ok {
		t.Error("empty server list should be rejected")
	}

	// remove.
	post(map[string]any{"op": "remove", "net": "lan", "domain": "eng.internal"})
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].DNSAdvertise) != 0 {
		t.Errorf("forward not removed: %+v", got.Networks[0].DNSAdvertise)
	}
}

func TestHandleDNSReject(t *testing.T) {
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
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/dns", bytes.NewReader(b))
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

	if ok, _ := post(map[string]any{"op": "reject", "net": "lan", "domain": "Blocked.internal"})["ok"].(bool); !ok {
		t.Fatal("reject-add rejected")
	}
	got, _ := config.Load(cfgPath)
	if len(got.Networks[0].DNSReject) != 1 || got.Networks[0].DNSReject[0].Domain != "Blocked.internal" {
		t.Fatalf("reject not saved: %+v", got.Networks[0].DNSReject)
	}

	if ok, _ := post(map[string]any{"op": "reject-disable", "net": "lan", "domain": "blocked.internal"})["ok"].(bool); !ok {
		t.Fatal("reject-disable rejected (should match case-insensitively)")
	}
	got, _ = config.Load(cfgPath)
	if !got.Networks[0].DNSReject[0].Disabled {
		t.Fatal("reject entry should be disabled")
	}

	if ok, _ := post(map[string]any{"op": "reject-enable", "net": "lan", "domain": "blocked.internal"})["ok"].(bool); !ok {
		t.Fatal("reject-enable rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.Networks[0].DNSReject[0].Disabled {
		t.Fatal("reject entry should be enabled again")
	}

	post(map[string]any{"op": "reject-remove", "net": "lan", "domain": "blocked.internal"})
	got, _ = config.Load(cfgPath)
	if len(got.Networks[0].DNSReject) != 0 {
		t.Errorf("reject entry not removed: %+v", got.Networks[0].DNSReject)
	}
}

// TestHandleDNSSearchOpRemoved covers that search-add/search-remove are no
// longer valid ops on /api/dns: search domains aren't a separately managed
// list anymore, they're derived from each advertised domain's own string
// (see cmd/gravinet's spec.SearchDomains construction), so there is nothing
// left for these ops to do.
func TestHandleDNSSearchOpRemoved(t *testing.T) {
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
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/dns", bytes.NewReader(b))
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

	if ok, _ := post(map[string]any{"op": "search-add", "net": "lan", "domain": "corp.internal"})["ok"].(bool); ok {
		t.Fatal("search-add should be rejected as an unknown op")
	}
	if ok, _ := post(map[string]any{"op": "search-remove", "net": "lan", "domain": "corp.internal"})["ok"].(bool); ok {
		t.Fatal("search-remove should be rejected as an unknown op")
	}
}
