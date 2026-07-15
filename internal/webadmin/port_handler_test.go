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

// handlePort takes a list of ports: the first becomes the primary, any rest
// become extra listen-only ports. Writes both to the config and triggers a
// reload (the live rebind itself is covered by the transport package's own
// TestExtraPortListenAndReply / TestTLSExtraPortAcceptAndReply).
func TestHandlePortChangesConfigAndReloads(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/port", bytes.NewReader(b))
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

	// single-port list: just changes the primary, same as before this list
	// shape existed.
	out := post(map[string]any{"ports": []int{51821}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("port change rejected: %v", out)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryPort != 51821 {
		t.Errorf("config PrimaryPort = %d, want 51821", got.PrimaryPort)
	}
	if len(got.ExtraListenPorts) != 0 {
		t.Errorf("config ExtraListenPorts = %v, want empty", got.ExtraListenPorts)
	}
	if reloads == 0 {
		t.Error("reload was not triggered")
	}

	// multi-port list: first is primary, rest are extras.
	out = post(map[string]any{"ports": []int{51822, 443, 80}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("port list change rejected: %v", out)
	}
	got, _ = config.Load(cfgPath)
	if got.PrimaryPort != 51822 {
		t.Errorf("config PrimaryPort = %d, want 51822", got.PrimaryPort)
	}
	if len(got.ExtraListenPorts) != 2 || got.ExtraListenPorts[0] != 443 || got.ExtraListenPorts[1] != 80 {
		t.Errorf("config ExtraListenPorts = %v, want [443 80]", got.ExtraListenPorts)
	}

	// invalid port anywhere in the list rejects the whole thing, config unchanged
	out = post(map[string]any{"ports": []int{51823, 70000}})
	if ok, _ := out["ok"].(bool); ok {
		t.Error("out-of-range port in the list should be rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.PrimaryPort != 51822 {
		t.Errorf("config PrimaryPort changed on invalid input: %d", got.PrimaryPort)
	}

	// empty list rejected — a primary port is mandatory
	out = post(map[string]any{"ports": []int{}})
	if ok, _ := out["ok"].(bool); ok {
		t.Error("an empty port list should be rejected")
	}
	got, _ = config.Load(cfgPath)
	if got.PrimaryPort != 51822 {
		t.Errorf("config PrimaryPort changed on empty list: %d", got.PrimaryPort)
	}
}

// TestHandlePortDisableInteraction covers the "-" sentinel (sent as
// {disabled:true}): turning UDP off must clear PrimaryPort/ExtraListenPorts,
// must be refused while the TCP/TLS fallback is also off (a node can't have
// neither), and re-enabling UDP with a normal port list must work again
// afterward. The TCP side of the same interaction is exercised at the config
// layer by TestValidatePrimaryPortZeroRequiresTCPFallback; this test is the
// end-to-end HTTP-handler version of the UDP side specifically, including
// the case where handleTCPPort's own disable is what's refused because UDP
// is the one currently off.
func TestHandlePortDisableInteraction(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
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
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	postTo := func(path string, body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
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

	// Turning UDP off while the TCP fallback is on (the default) must
	// succeed and clear both PrimaryPort and any extras.
	out := postTo("/api/port", map[string]any{"disabled": true})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("disabling udp with tcp fallback enabled should succeed: %v", out)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryPort != 0 {
		t.Errorf("config PrimaryPort = %d, want 0 after disabling udp", got.PrimaryPort)
	}
	if len(got.ExtraListenPorts) != 0 {
		t.Errorf("config ExtraListenPorts = %v, want empty after disabling udp", got.ExtraListenPorts)
	}

	// With UDP now off, disabling the TCP fallback too must be refused —
	// the node would have no way to be reached.
	out = postTo("/api/tcpport", map[string]any{"disabled": true})
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("disabling tcp fallback while udp is already off should be refused")
	}
	got, _ = config.Load(cfgPath)
	if got.DisableTCPFallback {
		t.Error("tcp fallback should still be enabled after the refused disable")
	}

	// Re-enabling UDP with a normal port list must work again, and clears
	// the disabled state (posting a port list, not {disabled:true}).
	out = postTo("/api/port", map[string]any{"ports": []int{51820}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("re-enabling udp with a port list should succeed: %v", out)
	}
	got, _ = config.Load(cfgPath)
	if got.PrimaryPort != 51820 {
		t.Errorf("config PrimaryPort = %d, want 51820 after re-enabling udp", got.PrimaryPort)
	}

	// Now that both are on again, disabling the TCP fallback must succeed.
	out = postTo("/api/tcpport", map[string]any{"disabled": true})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("disabling tcp fallback with udp enabled should succeed: %v", out)
	}
	got, _ = config.Load(cfgPath)
	if !got.DisableTCPFallback {
		t.Error("tcp fallback should be disabled")
	}

	// And with the TCP fallback now off, disabling UDP too must be refused.
	out = postTo("/api/port", map[string]any{"disabled": true})
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("disabling udp while tcp fallback is already off should be refused")
	}
	got, _ = config.Load(cfgPath)
	if got.PrimaryPort != 51820 {
		t.Errorf("config PrimaryPort = %d, want unchanged (51820) after the refused disable", got.PrimaryPort)
	}
}
