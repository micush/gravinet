package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// This file deliberately never sets up srv.upg (leaves it nil), and so
// never exercises "op":"run_now"'s success path — which, if reached, calls
// service.RunOSUpdateNow for real, which calls the host's actual package
// manager. This sandbox has a real apt-get on PATH; a test that reached
// that path wouldn't be exercising a mock, it would run a real `apt-get
// upgrade` as a side effect of the test suite. With srv.upg nil, "run_now"
// takes the exact same "upgrade machinery failed to initialize" rejection
// handleUpgradeHome's own tests already rely on being safe — a real,
// legitimate code path (a node whose upgrade state directory couldn't be
// created), not a workaround invented just for this test file. The same
// discipline as sysusers_test.go/groups_test.go/systemsnmp_handler_test.go/
// systeml2disco_handler_test.go, applied here to a package manager instead
// of useradd/groupadd/systemctl.

func osUpdatesTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443", AuthMode: "local"},
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
	// srv.upg is deliberately left nil — see the file-level comment above.
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

func TestUpgradeOSUpdatesGet(t *testing.T) {
	ts, c := osUpdatesTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/upgrade/os-updates", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/upgrade/os-updates = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"enabled", "cadence", "weekday", "day_of_month", "hour", "minute", "supported", "hint", "running"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if enabled, _ := out["enabled"].(bool); enabled {
		t.Error("a fresh config should report enabled:false")
	}
	// srv.upg is nil in this test server, so there's no state directory to
	// read a last run from — last_run/last_ok/etc. should simply be absent,
	// not present-but-zero, matching "never run" versus "ran and we don't
	// know the details" being different answers.
	if _, ok := out["last_run"]; ok {
		t.Error("last_run should be absent when there is no upgrade state directory to read one from")
	}
}

func TestUpgradeOSUpdatesRejectsBadRequests(t *testing.T) {
	ts, c := osUpdatesTestServer(t)
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/upgrade/os-updates", strings.NewReader(string(b)))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown op", map[string]any{"op": "wipe"}, "op must be"},
		{"missing op", map[string]any{"enabled": true}, "op must be"},
		{"bad cadence", map[string]any{"op": "save", "enabled": true, "cadence": "hourly"}, "cadence must be"},
		{"weekday out of range", map[string]any{"op": "save", "enabled": true, "cadence": "weekly", "weekday": 9}, "weekday must be"},
		{"day of month too high", map[string]any{"op": "save", "enabled": true, "cadence": "monthly", "day_of_month": 31}, "day_of_month must be"},
		{"day of month zero", map[string]any{"op": "save", "enabled": true, "cadence": "monthly", "day_of_month": 0}, "day_of_month must be"},
		{"hour out of range", map[string]any{"op": "save", "enabled": true, "cadence": "daily", "hour": 24, "minute": 0}, "hour must be"},
		{"minute out of range", map[string]any{"op": "save", "enabled": true, "cadence": "daily", "hour": 3, "minute": 60}, "hour must be"},
		{
			// The safety-motivated case: with srv.upg nil, run_now is
			// refused before ever reaching a real package manager — see
			// the file-level comment.
			name: "run_now with no upgrade machinery initialized",
			body: map[string]any{"op": "run_now"},
			want: "upgrade machinery failed to initialize",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := post(tc.body)
			if status == 200 {
				t.Fatalf("expected a rejection, got 200: %#v", out)
			}
			msg, _ := out["error"].(string)
			if msg == "" {
				t.Fatal("a rejection must carry an error message the page can show")
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// TestUpgradeOSUpdatesSaveValidConfig checks a legitimate save actually
// takes — this only ever touches config.json (via mutateConfig) and never
// reaches a package manager at all, so it's safe regardless of srv.upg.
func TestUpgradeOSUpdatesSaveValidConfig(t *testing.T) {
	ts, c := osUpdatesTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"op": "save", "enabled": true, "cadence": "weekly", "weekday": 2, "hour": 4, "minute": 30,
	})
	req, _ := http.NewRequest("POST", ts.URL+"/api/upgrade/os-updates", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("valid save = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("expected ok:true, got %#v", out)
	}
	if cadence, _ := out["cadence"].(string); cadence != "weekly" {
		t.Errorf("cadence = %v, want weekly", out["cadence"])
	}
	if _, ok := out["next_run"]; !ok {
		t.Error("an enabled schedule should report a next_run")
	}
}

// TestUpgradeOSUpdatesIsLocalOnly guards the placement decision: unlike
// Power/Time/Users/SNMP/L2 Disco, this endpoint deliberately does NOT
// follow the selected node — it's grouped with the rest of System >
// Upgrade's already-local-only endpoints (see handleUpgradeOSUpdates' own
// doc comment for why).
func TestUpgradeOSUpdatesIsLocalOnly(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if !strings.Contains(local, "/api/upgrade/os-updates") {
		t.Error("/api/upgrade/os-updates must be in LOCAL_API, matching the rest of the Upgrade page")
	}
}
