package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// fwPost logs in (via the supplied session cookie) and POSTs a firewall op,
// returning the decoded JSON body.
func fwPost(t *testing.T, ts *httptest.Server, cookie *http.Cookie, payload map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", ts.URL+"/api/firewall", bytes.NewReader(b))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// TestFirewallAddSingleCallNoRestart locks in two fixes: adding a rule hits the
// engine exactly once (the broken handler also persisted via mutateConfig which,
// combined with the engine's synchronous persist hook, duplicated every rule),
// and the add reports restart:false because it applies live.
func TestFirewallAddSingleCallNoRestart(t *testing.T) {
	_, be, ts := newTestServer(t)
	c := sessionFor(t, ts)

	out := fwPost(t, ts, c, map[string]any{
		"net": "1234", "op": "add", "at": -1,
		"rule": map[string]any{"action": "deny", "proto": "tcp", "dport_min": 22, "dport_max": 22},
	})

	if be.fwAddCalls != 1 {
		t.Fatalf("add should reach the engine exactly once, got %d", be.fwAddCalls)
	}
	if r, _ := out["restart"].(bool); r {
		t.Fatal("adding a rule applies live; restart must be false")
	}
}

// TestFirewallTogglesAreLive verifies the whole-firewall and per-rule enable/
// disable operations report restart:false — they apply through the live reload,
// so the UI must not nag the user to restart.
func TestFirewallTogglesAreLive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820,
		EnableIPv4:  true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Firewall: config.Firewall{Enabled: true, Rules: []config.FirewallRule{
				{Action: "deny", Proto: "tcp", DstPortMin: 22, DstPortMax: 22},
			}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{
		AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900},
	}
	be := &stubBackend{fwRules: []mesh.FirewallRule{
		{ID: 1, Action: "deny", Proto: "tcp", DstPortMin: 22, DstPortMax: 22},
	}}
	srv := New(wcfg, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil }) // reload itself is covered by the mesh live-reload test
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	for _, op := range []string{"disable", "enable"} {
		out := fwPost(t, ts, c, map[string]any{"net": "1234", "op": op})
		if out["error"] != nil {
			t.Fatalf("firewall %s errored: %v", op, out["error"])
		}
		if r, _ := out["restart"].(bool); r {
			t.Fatalf("firewall %s applies live; restart must be false", op)
		}
	}
	for _, op := range []string{"rule-disable", "rule-enable"} {
		out := fwPost(t, ts, c, map[string]any{"net": "1234", "op": op, "ids": []int{1}})
		if out["error"] != nil {
			t.Fatalf("%s errored: %v", op, out["error"])
		}
		if r, _ := out["restart"].(bool); r {
			t.Fatalf("%s applies live; restart must be false", op)
		}
	}
}

// TestFirewallObjectsServicesCounters covers the v392 catalog ops wired into the
// web admin: setting the object catalog, the service catalog, and resetting hit
// counters all reach the backend and report restart:false (they apply live).
func TestFirewallObjectsServicesCounters(t *testing.T) {
	_, be, ts := newTestServer(t)
	c := sessionFor(t, ts)

	out := fwPost(t, ts, c, map[string]any{
		"net": "1234", "op": "objects",
		"objects": []map[string]any{
			{"name": "web", "kind": "host", "addresses": []string{"10.0.0.10"}},
			{"name": "grp", "kind": "group", "members": []string{"web"}},
		},
	})
	if out["error"] != nil {
		t.Fatalf("objects op errored: %v", out["error"])
	}
	if be.fwObjSetCalls != 1 || len(be.fwObjects) != 2 {
		t.Fatalf("SetFirewallObjects not applied: calls=%d objs=%d", be.fwObjSetCalls, len(be.fwObjects))
	}
	if r, _ := out["restart"].(bool); r {
		t.Fatal("objects op applies live; restart must be false")
	}

	out = fwPost(t, ts, c, map[string]any{
		"net": "1234", "op": "services",
		"services": []map[string]any{
			{"name": "dns", "ports": []map[string]any{{"proto": "udp", "port_min": 53}, {"proto": "tcp", "port_min": 53}}},
		},
	})
	if out["error"] != nil {
		t.Fatalf("services op errored: %v", out["error"])
	}
	if be.fwSvcSetCalls != 1 || len(be.fwServices) != 1 || len(be.fwServices[0].Ports) != 2 {
		t.Fatalf("SetFirewallServices not applied: calls=%d svcs=%v", be.fwSvcSetCalls, be.fwServices)
	}

	out = fwPost(t, ts, c, map[string]any{"net": "1234", "op": "reset-counters", "ids": []uint64{}})
	if out["error"] != nil {
		t.Fatalf("reset-counters op errored: %v", out["error"])
	}
	// Empty ids reaches the backend as a request to reset all.
	if be.fwResetCounterCallCount != 1 {
		t.Fatal("FirewallResetCounters was not called")
	}
}
