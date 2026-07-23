package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// timeTestServer stands up an authenticated admin server for the System > Time
// endpoint. It needs no network config — /api/system/time reads host state, not
// gravinet's config, which is the whole design (see hosttime.go).
func timeTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
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
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

// TestSystemTimeGet checks the read shape the page draws from. Every field the UI
// touches must be present, since a missing one renders as "undefined" rather
// than failing visibly.
func TestSystemTimeGet(t *testing.T) {
	ts, c := timeTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/time", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/time = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"now", "now_ms", "timezone", "timezone_style", "abbrev", "offset_seconds",
		"ntp_enabled", "ntp_known", "synchronized", "sync_known", "servers", "manager",
		"can_timezone", "can_ntp", "can_clock", "hint", "skew_tolerance_seconds"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	// servers must always be an array, never null: the UI joins it.
	if _, ok := out["servers"].([]any); !ok {
		t.Errorf("servers = %#v, want a JSON array even when empty", out["servers"])
	}
	// The tolerance shown to the operator has to be the one the engine enforces,
	// not a hardcoded copy — that's why mesh exports it.
	if got, want := out["skew_tolerance_seconds"], mesh.ClockSkewTolerance().Seconds(); got != want {
		t.Errorf("skew_tolerance_seconds = %v, want %v (mesh.ClockSkewTolerance)", got, want)
	}
	if out["timezone_style"] != "iana" && out["timezone_style"] != "windows" {
		t.Errorf("timezone_style = %v, want iana or windows", out["timezone_style"])
	}
}

// TestSystemTimeRejectsBadRequests covers the refusal paths. Nothing here should
// reach an OS command, so running the suite can't change the test machine's
// clock or timezone.
func TestSystemTimeRejectsBadRequests(t *testing.T) {
	ts, c := timeTestServer(t)
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/time", bytes.NewReader(b))
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
		want string // substring the error should contain
	}{
		{"unknown op", map[string]any{"op": "nonsense"}, "op must be"},
		{"missing op", map[string]any{}, "op must be"},
		{"empty timezone", map[string]any{"op": "timezone", "timezone": ""}, "empty"},
		{"timezone traversal", map[string]any{"op": "timezone", "timezone": "../../etc/shadow"}, "traversal"},
		{"timezone injection", map[string]any{"op": "timezone", "timezone": "UTC; reboot"}, "aren't allowed"},
		{"ntp server injection", map[string]any{"op": "ntp", "enabled": true, "servers": []string{"a.example", "b; reboot"}}, "aren't allowed"},
		{"clock unparseable", map[string]any{"op": "clock", "datetime": "tomorrow-ish"}, "must look like"},
		{"clock empty", map[string]any{"op": "clock", "datetime": ""}, "no date and time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := post(tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (reply: %v)", code, out)
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

// TestSystemTimeIsProxyable guards the placement decision: like Power, this
// endpoint follows the selected node, so it must NOT be in the client's
// LOCAL_API pin-to-this-node list and must not be in handleProxy's blocklist.
// Getting either wrong would silently apply a fix to the wrong host's clock —
// the exact class of bug LOCAL_API's own comment describes.
func TestSystemTimeIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/time") {
		t.Error("/api/system/time is pinned in LOCAL_API; it should follow the selected node like /api/system/power")
	}
	if strings.Contains(local, "/api/system/power") {
		t.Error("/api/system/power appeared in LOCAL_API — this test's premise (both follow the target) no longer holds")
	}
}

// TestSystemTimeNavPlacement pins the page into System, above Power. Power being
// last in the group is deliberate (it's the item that takes the host down), and
// the nav is data-driven from NAV_GROUPS, so this is the only place the ordering
// is expressed.
func TestSystemTimeNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'upgrade'", "'time'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'time'") > strings.Index(block, "'power'") {
		t.Error("Power must stay last in the System group; new items go above it")
	}
	infoBlock := indexHTML[strings.Index(indexHTML, "{ name:'info'"):]
	infoBlock = infoBlock[:strings.Index(infoBlock, "]},")]
	if strings.Contains(infoBlock, "'time'") {
		t.Error("time leaked into the Info group")
	}
	// The section has to be dispatchable, or clicking the rail entry renders
	// nothing and the console looks broken.
	if !strings.Contains(indexHTML, "time:secTime") {
		t.Error("no time:secTime entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secTime(") {
		t.Error("secTime is not defined")
	}
}
