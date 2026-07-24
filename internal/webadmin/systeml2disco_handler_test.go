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

// l2discoTestServer stands up an authenticated admin server for the
// System > L2 Disco endpoint. Mirrors snmpTestServer's/usersTestServer's shape.
func l2discoTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
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
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

// TestSystemL2DiscoGet checks the read shape the page draws from. Like
// SNMP's GET, this is read-only all the way down: service.LLDPServiceRunning
// and service.LLDPNeighbors both only ever *query* live state (systemctl
// is-active, lldpcli show neighbors), never mutate it, so this is safe
// regardless of whether lldpd happens to be installed on the machine
// running the test suite.
func TestSystemL2DiscoGet(t *testing.T) {
	ts, c := l2discoTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/l2disco", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/l2disco = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"interfaces", "supported", "hint", "running", "neighbors", "neighbors_available", "neighbors_hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if _, ok := out["interfaces"].([]any); !ok {
		t.Errorf("interfaces = %#v, want a JSON array even when empty", out["interfaces"])
	}
	if _, ok := out["neighbors"].([]any); !ok {
		t.Errorf("neighbors = %#v, want a JSON array even when empty", out["neighbors"])
	}
}

// TestSystemL2DiscoRejectsInvalidIfaceName is the one POST case this suite
// covers, deliberately, for the same reason
// TestSystemSNMPRejectsEnabledWithoutCommunity is the only POST case that
// suite covers: it's a request guaranteed to be refused by validation
// *before* the handler ever reaches service.ApplyLLDP, which — for any
// request that gets past validation, including an empty {"interfaces":[]}
// body — goes on to really call systemctl/service/rcctl/brew services to
// enable, restart, or disable the actual lldpd service. A successful save
// (even one that disables the agent) is not tested here for exactly that
// reason: on a machine where an "lldpd" service happens to already exist,
// exercising that path for real would toggle it as a side effect of
// running this test suite.
//
// Unlike SNMP's "community required" rule (a real product constraint,
// enabled needs a value to be enabled), an invalid interface name has no
// equivalent "this combination is nonsensical" story — any single field
// can be wrong on its own. This checks the interface-name character-class
// validation specifically, since that's the one guaranteed to reject
// before mutateConfig/ApplyLLDP are ever reached, and because it doubles
// as the same injection-resistance boundary
// service.ValidLLDPIface/TestLLDPValidIface already covers at the argv
// layer, now confirmed at the HTTP layer too.
func TestSystemL2DiscoRejectsInvalidIfaceName(t *testing.T) {
	ts, c := l2discoTestServer(t)
	body, _ := json.Marshal(map[string]any{"interfaces": []map[string]any{
		{"name": "eth0; rm -rf /", "lldp": true, "cdp": false},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/l2disco", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("an invalid interface name must be rejected")
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "invalid interface name") {
		t.Errorf("error = %q, want it to mention the invalid interface name", msg)
	}
}

// TestSystemL2DiscoIsProxyable guards the placement decision: like the
// other System > * endpoints, this follows the selected node, so it must
// NOT be in the client's LOCAL_API pin-to-this-node list.
func TestSystemL2DiscoIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/l2disco") {
		t.Error("/api/system/l2disco is pinned in LOCAL_API; it should follow the selected node like the other System > * endpoints")
	}
}

// TestReconcileL2DiscoNeighborsHint pins the exact bug reported against a
// real page: a green "running" tag next to "lldpd is not running" —
// individually truthful about what each check found, flatly contradicting
// each other once shown together. running/neighborsAvailable disagreeing
// (the interesting case) must produce the reconciled explanation, not
// either raw claim; every other combination must pass the raw hint
// through unchanged.
func TestReconcileL2DiscoNeighborsHint(t *testing.T) {
	cases := []struct {
		name                        string
		running, neighborsAvailable bool
		rawHint, wantContains       string
		wantExactPassthrough        bool
	}{
		{
			name:    "the reported bug: active but unreachable",
			running: true, neighborsAvailable: false,
			rawHint:      "could not connect to lldpd's control interface",
			wantContains: "reports active",
		},
		{
			name:    "genuinely not running: passthrough",
			running: false, neighborsAvailable: false,
			rawHint:              "lldpd is not installed",
			wantExactPassthrough: true,
		},
		{
			name:    "everything fine: passthrough (empty)",
			running: true, neighborsAvailable: true,
			rawHint:              "",
			wantExactPassthrough: true,
		},
		{
			name:    "not running and somehow available (shouldn't happen, but must not fabricate a claim)",
			running: false, neighborsAvailable: true,
			rawHint:              "",
			wantExactPassthrough: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileL2DiscoNeighborsHint(c.running, c.neighborsAvailable, c.rawHint)
			if c.wantExactPassthrough {
				if got != c.rawHint {
					t.Errorf("got %q, want the raw hint %q passed through unchanged", got, c.rawHint)
				}
				return
			}
			if !strings.Contains(got, c.wantContains) {
				t.Errorf("got %q, want it to contain %q", got, c.wantContains)
			}
			// The reconciled hint must never repeat the contradicted raw
			// claim ("not running") right next to a running:true tag.
			if strings.Contains(strings.ToLower(got), "not running") {
				t.Errorf("reconciled hint still says \"not running\" while running=true: %q", got)
			}
		})
	}
}

// TestSystemL2DiscoNavPlacement pins L2 Disco into the System group after
// SNMP and before Users.
func TestSystemL2DiscoNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'snmp'", "'l2disco'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'snmp'") > strings.Index(block, "'l2disco'") {
		t.Error("L2 Disco must come after SNMP in the System group")
	}
	if strings.Index(block, "'l2disco'") > strings.Index(block, "'users'") {
		t.Error("L2 Disco must come before Users in the System group")
	}
	if !strings.Contains(indexHTML, "l2disco:secL2Disco") {
		t.Error("no l2disco:secL2Disco entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secL2Disco(") {
		t.Error("secL2Disco is not defined")
	}
	if !strings.Contains(indexHTML, "l2disco') return 'L2 Disco'") {
		t.Error("label() has no l2disco -> 'L2 Disco' case")
	}
}
