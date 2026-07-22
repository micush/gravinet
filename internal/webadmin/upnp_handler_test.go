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

func TestHandleUPnPSetting(t *testing.T) {
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/upnp", bytes.NewReader(b))
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

	// Off by default (config.Config.EnableUPnP's doc comment) — cfg above
	// never sets it, and the omitempty JSON tag means it's simply absent
	// from the saved file, decoding back to the zero value (false).
	if v, _ := get()["enable_upnp"].(bool); v {
		t.Fatal("enable_upnp should start false (off by default)")
	}

	// Toggling on persists to disk, requests a restart, and triggers
	// reload. Unlike GeoIP's Server-scoped s.cfg (frozen at New()),
	// enable_upnp is read fresh from disk on every /api/config call (see
	// handleConfig's config.Load), so it's expected to already reflect the
	// saved value even before an actual restart — what needs the restart
	// is the upnp.Manager itself being started, not this reported value.
	res := post(true)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/upnp on=true rejected: %v", res)
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
	if !got.EnableUPnP {
		t.Fatal("EnableUPnP not persisted to disk as true")
	}
	if v, _ := get()["enable_upnp"].(bool); !v {
		t.Error("/api/config should report the just-saved (true) value")
	}

	// Toggling back off also persists correctly.
	res = post(false)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/upnp on=false rejected: %v", res)
	}
	got, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnableUPnP {
		t.Fatal("EnableUPnP not persisted to disk as false")
	}
	if v, _ := get()["enable_upnp"].(bool); v {
		t.Error("/api/config should report the just-saved (false) value")
	}
}
