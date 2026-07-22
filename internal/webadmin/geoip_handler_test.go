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

func TestHandleGeoIPSetting(t *testing.T) {
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/geoip", bytes.NewReader(b))
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

	// wcfg above never mentions GeoIPLookup, so it starts nil — meaning "on"
	// (see GeoIPEnabled's doc comment) — before any toggle at all.
	if v, _ := get()["geoip_lookup"].(bool); !v {
		t.Fatal("geoip_lookup should start true (unset defaults to enabled)")
	}

	// Toggling off persists to disk, requests a restart, and triggers
	// reload — but must NOT flip live: unlike NAT state timeout (a
	// mesh-level setting the running Engine picks up), this is a
	// webadmin.Server-scoped setting (s.cfg, captured once at New()), so
	// /api/config should keep reporting the pre-toggle (true) value until an
	// actual restart.
	res := post(false)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/geoip on=false rejected: %v", res)
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
	if got.WebAdmin.GeoIPEnabled() {
		t.Fatal("GeoIPLookup not persisted to disk as false")
	}
	if v, _ := get()["geoip_lookup"].(bool); !v {
		t.Error("/api/config should still report the pre-restart (true) value — this setting isn't live-applied")
	}

	// Toggling back on also persists correctly.
	res = post(true)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("POST /api/geoip on=true rejected: %v", res)
	}
	got, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WebAdmin.GeoIPEnabled() {
		t.Fatal("GeoIPLookup not persisted to disk as true")
	}
}
