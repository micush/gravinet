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

func TestHandleIPForwardingSetting(t *testing.T) {
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

	post := func(on bool) map[string]any {
		b, _ := json.Marshal(map[string]any{"on": on})
		req, _ := http.NewRequest("POST", ts.URL+"/api/ipforwarding", bytes.NewReader(b))
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
	get := func() map[string]any {
		req, _ := http.NewRequest("GET", ts.URL+"/api/config", nil)
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

	// On by default (config.Config.ForwardingEnabled's doc comment) — cfg
	// above never sets IPForwarding, so it's nil, which ForwardingEnabled
	// treats as enabled.
	if v, _ := get()["ip_forwarding"].(bool); !v {
		t.Fatal("ip_forwarding should start true (unset defaults to enabled)")
	}

	// Toggling off persists to disk, requests a restart, and triggers
	// reload. Like enable_upnp (and unlike GeoIP's Server-scoped s.cfg),
	// ip_forwarding is read fresh from disk on every /api/config call, so
	// it's expected to already reflect the saved value even before an
	// actual restart — what needs the restart is the host's own
	// forwarding sysctl being flipped, not this reported value.
	res := post(false)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/ipforwarding on=false rejected: %v", res)
	}
	if restart, _ := res["restart"].(bool); !restart {
		t.Error("expected restart:true in the response")
	}
	if reloads == 0 {
		t.Error("reload not triggered")
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ForwardingEnabled() {
		t.Fatal("IPForwarding not persisted to disk as false")
	}
	if v, _ := get()["ip_forwarding"].(bool); v {
		t.Error("/api/config should report the just-saved (false) value")
	}

	// Toggling back on also persists correctly, and explicitly (not just
	// reverting to nil) — ForwardingEnabled treats both the same, but the
	// on-disk representation should be the explicit true this specific
	// save wrote, not an absent field left over from before.
	res = post(true)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/ipforwarding on=true rejected: %v", res)
	}
	got, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.IPForwarding == nil || !*got.IPForwarding {
		t.Fatalf("IPForwarding not persisted to disk as explicit true: %+v", got.IPForwarding)
	}
	if v, _ := get()["ip_forwarding"].(bool); !v {
		t.Error("/api/config should report the just-saved (true) value")
	}
}
