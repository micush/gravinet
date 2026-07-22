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

// handleTCPPort is handlePort's TCP/TLS-fallback counterpart: the first port
// in the list becomes the fallback port itself, any rest become extra
// TCP/TLS listen ports. Writes both to the config and triggers a reload (the
// live rebind itself is covered by the transport package's own
// TestTLSExtraPortAcceptAndReply / TestTLSExtraPortBadPortSkipped).
func TestHandleTCPPortChangesConfigAndReloads(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Firewall: config.Firewall{Enabled: true}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/tcpport", bytes.NewReader(b))
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

	// single-port list: just changes the fallback port, same as before this
	// list shape existed.
	out := post(map[string]any{"ports": []int{443}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("tcp port change rejected: %v", out)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.TCPFallbackPort != 443 {
		t.Errorf("config TCPFallbackPort = %d, want 443", got.TCPFallbackPort)
	}
	if len(got.ExtraTCPListenPorts) != 0 {
		t.Errorf("config ExtraTCPListenPorts = %v, want empty", got.ExtraTCPListenPorts)
	}
	if reloads == 0 {
		t.Error("reload was not triggered")
	}

	// multi-port list: first is the fallback port, rest are extras.
	out = post(map[string]any{"ports": []int{65432, 21, 80}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("tcp port list change rejected: %v", out)
	}
	got, _ = config.Load(cfgPath)
	if got.TCPFallbackPort != 65432 {
		t.Errorf("config TCPFallbackPort = %d, want 65432", got.TCPFallbackPort)
	}
	if len(got.ExtraTCPListenPorts) != 2 || got.ExtraTCPListenPorts[0] != 21 || got.ExtraTCPListenPorts[1] != 80 {
		t.Errorf("config ExtraTCPListenPorts = %v, want [21 80]", got.ExtraTCPListenPorts)
	}

	// invalid port anywhere in the list rejects the whole thing, config unchanged
	out = post(map[string]any{"ports": []int{443, 70000}})
	if ok, _ := out["ok"].(bool); ok {
		t.Error("out-of-range tcp port in the list should be rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.TCPFallbackPort != 65432 {
		t.Errorf("config TCPFallbackPort changed on invalid input: %d", got.TCPFallbackPort)
	}

	// empty list rejected — a fallback port is mandatory
	out = post(map[string]any{"ports": []int{}})
	if ok, _ := out["ok"].(bool); ok {
		t.Error("an empty port list should be rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.TCPFallbackPort != 65432 {
		t.Errorf("config TCPFallbackPort changed on empty list: %d", got.TCPFallbackPort)
	}
}
