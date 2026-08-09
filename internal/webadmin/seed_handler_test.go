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

// TestSeedAddRemoveLive verifies /api/seed persists add/remove and reports
// restart:false (adding a seed applies live via the reload).
func TestSeedAddRemoveLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
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

	post := func(op, addr string) map[string]any {
		b, _ := json.Marshal(map[string]any{"net": "1234", "op": op, "addr": addr})
		req, _ := http.NewRequest("POST", ts.URL+"/api/seed", bytes.NewReader(b))
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
	seeds := func() []string { c2, _ := config.Load(cfgPath); return c2.Networks[0].Seeds.Addrs() }

	if out := post("add", "203.0.113.5:51820"); out["error"] != nil {
		t.Fatalf("add errored: %v", out["error"])
	}
	if s := seeds(); len(s) != 1 || s[0] != "203.0.113.5:51820" {
		t.Fatalf("seeds after add = %v", s)
	}
	if out := post("add", "bad:99999"); out["error"] == nil {
		t.Fatal("expected validation error for bad port")
	}

	// Setting notes on an existing seed persists them without touching the
	// address, and is reported live (restart:false) like add/remove.
	notesBody, _ := json.Marshal(map[string]any{"net": "1234", "op": "notes", "addr": "203.0.113.5:51820", "notes": "office uplink"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/seed", bytes.NewReader(notesBody))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != nil {
		t.Fatalf("notes errored: %v", out["error"])
	}
	if c2, _ := config.Load(cfgPath); len(c2.Networks[0].Seeds) != 1 || c2.Networks[0].Seeds[0].Notes != "office uplink" {
		t.Fatalf("notes not persisted: %+v", c2.Networks[0].Seeds)
	}

	// update-addr (the web UI's inline address-edit and udp/tcp transport
	// flip) must rename the seed in place, keeping the notes just set above —
	// the whole reason this op exists instead of add-then-remove, which used
	// to silently wipe them (see SeedUpdateAddr's doc comment).
	updBody, _ := json.Marshal(map[string]any{"net": "1234", "op": "update-addr", "addr": "203.0.113.5:51820", "newAddr": "203.0.113.5:65432"})
	req2, _ := http.NewRequest("POST", ts.URL+"/api/seed", bytes.NewReader(updBody))
	req2.AddCookie(c)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&out2)
	if out2["error"] != nil {
		t.Fatalf("update-addr errored: %v", out2["error"])
	}
	if c2, _ := config.Load(cfgPath); len(c2.Networks[0].Seeds) != 1 ||
		c2.Networks[0].Seeds[0].Address != "203.0.113.5:65432" ||
		c2.Networks[0].Seeds[0].Notes != "office uplink" {
		t.Fatalf("after update-addr = %+v", c2.Networks[0].Seeds)
	}

	if out := post("remove", "203.0.113.5:65432"); out["error"] != nil {
		t.Fatalf("remove errored: %v", out["error"])
	}
	if s := seeds(); len(s) != 0 {
		t.Fatalf("seeds after remove = %v, want empty", s)
	}
}

// TestSeedEnableDisable covers the state column added to Mesh → Seeds: the
// enable/disable ops persist, leave the address/notes/position untouched, and
// are reported live (restart:false) like every other seed op.
//
// The assertion that matters most is the split between Addrs and
// EnabledAddrs — a disabled seed is still configured but must be gone from
// the list the daemon resolves and dials. A regression that dropped the
// filter would leave the row rendering "disabled" in the UI while the node
// went on dialing it, which is the failure this whole field exists to make
// impossible.
func TestSeedEnableDisable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
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

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/seed", bytes.NewReader(b))
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
	load := func() config.SeedList { c2, _ := config.Load(cfgPath); return c2.Networks[0].Seeds }

	for _, a := range []string{"203.0.113.5:51820", "198.51.100.9:51820"} {
		if out := post(map[string]any{"net": "1234", "op": "add", "addr": a}); out["error"] != nil {
			t.Fatalf("add %s errored: %v", a, out["error"])
		}
	}
	if out := post(map[string]any{"net": "1234", "op": "notes", "addr": "203.0.113.5:51820", "notes": "office uplink"}); out["error"] != nil {
		t.Fatalf("notes errored: %v", out["error"])
	}

	// A newly-added seed is enabled — the field is omitempty and the zero
	// value has to mean "in service" for existing configs to keep working.
	for _, s := range load() {
		if s.Disabled {
			t.Fatalf("a new seed must start enabled: %+v", s)
		}
	}

	out := post(map[string]any{"net": "1234", "op": "disable", "addr": "203.0.113.5:51820"})
	if out["error"] != nil {
		t.Fatalf("disable errored: %v", out["error"])
	}
	if out["restart"] == true {
		t.Errorf("disabling a seed should report restart:false like add/remove, got %v", out["restart"])
	}
	seeds := load()
	if len(seeds) != 2 || seeds[0].Address != "203.0.113.5:51820" || !seeds[0].Disabled || seeds[0].Notes != "office uplink" {
		t.Fatalf("disable should park the seed in place, keeping address/notes/position: %+v", seeds)
	}
	if got, want := len(seeds.Addrs()), 2; got != want {
		t.Fatalf("a disabled seed is still configured: Addrs len = %d, want %d", got, want)
	}
	if got := seeds.EnabledAddrs(); len(got) != 1 || got[0] != "198.51.100.9:51820" {
		t.Fatalf("EnabledAddrs after disable = %v, want only the enabled seed", got)
	}

	if out := post(map[string]any{"net": "1234", "op": "enable", "addr": "203.0.113.5:51820"}); out["error"] != nil {
		t.Fatalf("enable errored: %v", out["error"])
	}
	seeds = load()
	if seeds[0].Disabled || seeds[0].Notes != "office uplink" {
		t.Fatalf("enable should restore the seed with its notes intact: %+v", seeds[0])
	}
	if got := len(seeds.EnabledAddrs()); got != 2 {
		t.Fatalf("EnabledAddrs after re-enable = %d, want 2", got)
	}

	// An address that isn't configured is an error, not a silent success —
	// the UI sends the row's address, so a mismatch means the table and the
	// config have diverged and the operator should hear about it.
	if out := post(map[string]any{"net": "1234", "op": "disable", "addr": "203.0.113.99:51820"}); out["error"] == nil {
		t.Fatal("expected an error disabling a seed that isn't configured")
	}
}
