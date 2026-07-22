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

// TestPeerEnableDisableLive verifies the /api/peer endpoint: disabling a peer
// writes it to the network's local DisabledPeers list and reports restart:false
// (it applies live), and enabling removes it. This is the local-only counterpart
// to a ban.
func TestPeerEnableDisableLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
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

	post := func(node, op string) map[string]any {
		b, _ := json.Marshal(map[string]any{"net": "1234", "node": node, "op": op})
		req, _ := http.NewRequest("POST", ts.URL+"/api/peer", bytes.NewReader(b))
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
	disabledIn := func() []string {
		c2, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c2.Networks[0].DisabledPeers
	}

	out := post("nodeB", "disable")
	if out["error"] != nil {
		t.Fatalf("disable errored: %v", out["error"])
	}
	if r, _ := out["restart"].(bool); r {
		t.Fatal("disabling a peer applies live; restart must be false")
	}
	if dp := disabledIn(); len(dp) != 1 || dp[0] != "nodeB" {
		t.Fatalf("config DisabledPeers = %v, want [nodeB]", dp)
	}

	out = post("nodeB", "enable")
	if r, _ := out["restart"].(bool); r {
		t.Fatal("enabling a peer applies live; restart must be false")
	}
	if dp := disabledIn(); len(dp) != 0 {
		t.Fatalf("config DisabledPeers = %v, want empty after enable", dp)
	}

	// bad op is rejected
	if out = post("nodeB", "frobnicate"); out["error"] == nil {
		t.Fatal("expected error for invalid op")
	}
}
