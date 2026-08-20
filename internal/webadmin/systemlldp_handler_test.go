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

// lldpTestServer stands up an authenticated admin server for the
// System > LLDP endpoint. Mirrors snmpTestServer's/usersTestServer's shape.
func lldpTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
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
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

// TestSystemLLDPGet checks the read shape the page draws from. Like
// SNMP's GET, this is read-only all the way down: service.LLDPServiceRunning
// and service.LLDPNeighbors both only ever *query* live state (systemctl
// is-active, lldpcli show neighbors), never mutate it, so this is safe
// regardless of whether lldpd happens to be installed on the machine
// running the test suite.
func TestSystemLLDPGet(t *testing.T) {
	ts, c := lldpTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/lldp", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/lldp = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"interfaces", "enabled", "supported", "hint", "running", "neighbors", "neighbors_available", "neighbors_hint", "strays", "stray_hint"} {
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
	if _, ok := out["strays"].([]any); !ok {
		t.Errorf("strays = %#v, want a JSON array even when empty", out["strays"])
	}
}

// TestSystemLLDPRejectsInvalidIfaceName is the one POST case this suite
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
func TestSystemLLDPRejectsInvalidIfaceName(t *testing.T) {
	ts, c := lldpTestServer(t)
	body, _ := json.Marshal(map[string]any{"interfaces": []map[string]any{
		{"name": "eth0; rm -rf /", "lldp": true, "cdp": false},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/lldp", strings.NewReader(string(body)))
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

// TestSystemLLDPIsProxyable guards the placement decision: like the
// other System > * endpoints, this follows the selected node, so it must
// NOT be in the client's LOCAL_API pin-to-this-node list.
func TestSystemLLDPIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/lldp") {
		t.Error("/api/system/lldp is pinned in LOCAL_API; it should follow the selected node like the other System > * endpoints")
	}
}

// TestReconcileLLDPNeighborsHint pins the exact bug reported against a
// real page: a green "running" tag next to "lldpd is not running" —
// individually truthful about what each check found, flatly contradicting
// each other once shown together. running/neighborsAvailable disagreeing
// (the interesting case) must produce the reconciled explanation, not
// either raw claim; every other combination must pass the raw hint
// through unchanged. Also pins the follow-up fix: when a journalHint is
// available, the reconciled message uses it (a specific, confirmed-real
// root cause) instead of the generic list of maybes.
func TestReconcileLLDPNeighborsHint(t *testing.T) {
	cases := []struct {
		name                                    string
		running, neighborsAvailable, configured bool
		rawHint, journalHint                    string
		wantContains, wantNotContains           string
		wantExactPassthrough                    bool
	}{
		{
			name:    "the reported bug: active but unreachable, no journal detail",
			running: true, neighborsAvailable: false, configured: true,
			rawHint:      "could not connect to lldpd's control interface",
			wantContains: "reports active",
		},
		{
			name:    "active but unreachable, with a specific journal hint: use it, not the generic guess-list",
			running: true, neighborsAvailable: false, configured: true,
			rawHint:         "could not connect to lldpd's control interface",
			journalHint:     " — journal: another instance is running, please stop it (another lldpd instance, or a leftover control socket from one, may already be present — check `pgrep -a lldpd`...)",
			wantContains:    "another instance is running",
			wantNotContains: "It may still be starting up, or something",
		},
		{
			name:    "switched off: say so instead of a useless connect failure",
			running: false, neighborsAvailable: false, configured: false,
			rawHint:         "could not connect to lldpd's control interface",
			wantContains:    "no interfaces are picked",
			wantNotContains: "control interface",
		},
		{
			name:    "configured to run but genuinely not running: passthrough",
			running: false, neighborsAvailable: false, configured: true,
			rawHint:              "lldpd is not installed",
			wantExactPassthrough: true,
		},
		{
			name:    "everything fine: passthrough (empty)",
			running: true, neighborsAvailable: true, configured: true,
			rawHint:              "",
			wantExactPassthrough: true,
		},
		{
			name:    "not running and somehow available (shouldn't happen, but must not fabricate a claim)",
			running: false, neighborsAvailable: true, configured: true,
			rawHint:              "",
			wantExactPassthrough: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileLLDPNeighborsHint(c.running, c.neighborsAvailable, c.configured, c.rawHint, c.journalHint)
			if c.wantExactPassthrough {
				if got != c.rawHint {
					t.Errorf("got %q, want the raw hint %q passed through unchanged", got, c.rawHint)
				}
				return
			}
			if !strings.Contains(got, c.wantContains) {
				t.Errorf("got %q, want it to contain %q", got, c.wantContains)
			}
			if c.wantNotContains != "" && strings.Contains(got, c.wantNotContains) {
				t.Errorf("got %q, want it NOT to contain %q", got, c.wantNotContains)
			}
			// The reconciled hint must never repeat a claim that contradicts
			// the tag shown right next to it.
			if c.running && strings.Contains(strings.ToLower(got), "not running") {
				t.Errorf("reconciled hint still says \"not running\" while running=true: %q", got)
			}
		})
	}
}

// TestSystemLLDPNavPlacement pins LLDP into the System group after
// SNMP and before Users.
func TestSystemLLDPNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'snmp'", "'lldp'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'snmp'") > strings.Index(block, "'lldp'") {
		t.Error("LLDP must come after SNMP in the System group")
	}
	if strings.Index(block, "'lldp'") > strings.Index(block, "'users'") {
		t.Error("LLDP must come before Users in the System group")
	}
	if !strings.Contains(indexHTML, "lldp:secLLDP") {
		t.Error("no lldp:secLLDP entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secLLDP(") {
		t.Error("secLLDP is not defined")
	}
	// 'lldp' is an acronym section key like nat/qos/dns/bgp/api/snmp, so
	// label() must uppercase it through that shared branch rather than
	// title-casing it into "Lldp". Pinned on the ternary itself because
	// that branch, not a per-section case, is what renders the rail now.
	upper := indexHTML[strings.Index(indexHTML, "return s==='nat'"):]
	upper = upper[:strings.Index(upper, "\n")]
	if !strings.Contains(upper, "s==='lldp'") {
		t.Errorf("label()'s uppercase list is missing 'lldp' — the rail would read \"Lldp\":\n%s", upper)
	}
	// Before v892 the rail read the literal "l2disco" while the page's own
	// <h2> said "Layer 2 Discovery". "LLDP" is short enough to be both, so
	// sectionHeading() no longer special-cases this section at all and the
	// <h2> comes straight from label(). Scoped to the function body: the
	// old heading's name still legitimately appears elsewhere in the source
	// (parapet's own page is called "Network > L2 Discovery"), so a
	// whole-file search would match text this test has no business pinning.
	head := indexHTML[strings.Index(indexHTML, "function sectionHeading(s){"):]
	head = head[:strings.Index(head, "\n}")]
	if strings.Contains(head, "lldp") {
		t.Errorf("sectionHeading() still special-cases lldp; the <h2> should come from label():\n%s", head)
	}
}
