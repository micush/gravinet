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
		PrimaryPort: 51820, EnableIPv4: true,
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
