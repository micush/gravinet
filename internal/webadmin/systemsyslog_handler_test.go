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

// syslogTestServer stands up an authenticated admin server for the
// System > Syslog endpoint. Mirrors snmpTestServer's/timeTestServer's shape.
func syslogTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
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

// TestSystemSyslogGet checks the read shape the table draws from. GET is
// read-only all the way down — service.HostSyslog only probes
// (haveCmd/os.ReadFile), it never mutates anything — so this is safe
// regardless of whether rsyslog/syslogd happen to be installed on the
// machine running the test suite.
func TestSystemSyslogGet(t *testing.T) {
	ts, c := syslogTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/syslog", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/syslog = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"targets", "manager", "supported", "hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the table reads it directly", k)
		}
	}
	targets, _ := out["targets"].([]any)
	if len(targets) != 0 {
		t.Errorf("a fresh host should report an empty target list unless it already had gravinet-managed forwarding configured, got %v", targets)
	}
}

// TestSystemSyslogRejectsEmptyRemote is one of the POST cases this suite
// covers unconditionally, deliberately: validation (service.SetHostSyslog)
// runs before setLinuxSyslog/setBSDSyslog ever touch a real file or run a
// real command, so this is safe regardless of what's installed on the
// machine running the test suite — the same reasoning
// TestSystemSNMPRejectsEnabledWithoutCommunity's own doc comment gives for
// why this suite stops there rather than exercising a real save.
func TestSystemSyslogRejectsEmptyRemote(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"targets": []map[string]any{
		{"remote": "", "port": 514, "protocol": "udp"},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/syslog", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("a target with an empty remote must be rejected")
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["error"].(string); !ok {
		t.Errorf("reply = %#v, want an \"error\" field", out)
	}
}

// TestSystemSyslogRejectsBadProtocol is another request shape refused by
// validation before anything real is touched — same safety reasoning as
// TestSystemSyslogRejectsEmptyRemote above.
func TestSystemSyslogRejectsBadProtocol(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"targets": []map[string]any{
		{"remote": "log.example.com", "port": 514, "protocol": "quic"},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/syslog", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("an unrecognized protocol must be rejected")
	}
}

// TestSystemSyslogRejectsBadPort covers the port-range validation, the one
// field the old single-target combined "host:port" string used to check as
// part of parsing a single field — now a separate structured field that
// needs its own bounds check.
func TestSystemSyslogRejectsBadPort(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"targets": []map[string]any{
		{"remote": "log.example.com", "port": 70000, "protocol": "udp"},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/syslog", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("a port outside 1-65535 must be rejected")
	}
}

// TestSystemSyslogRejectsOneBadTargetAmongGoodOnes checks that a batch
// save is all-or-nothing: one invalid entry among several valid ones
// rejects the whole request rather than silently saving a partial list,
// so the admin never ends up with a save that "succeeded" but dropped an
// entry.
func TestSystemSyslogRejectsOneBadTargetAmongGoodOnes(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"targets": []map[string]any{
		{"remote": "log.example.com", "port": 514, "protocol": "udp"},
		{"remote": "log2.example.com", "port": 514, "protocol": "carrier-pigeon"},
	}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/syslog", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("one bad target among several must reject the whole batch")
	}
}

// TestSystemSyslogIsProxyable guards the placement decision: like
// Power/Time/Resolver/SNMP/LLDP/Users, this endpoint follows the
// selected node, so it must NOT be in the client's LOCAL_API
// pin-to-this-node list.
func TestSystemSyslogIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/syslog") {
		t.Error("/api/system/syslog is pinned in LOCAL_API; it should follow the selected node like the other System > * endpoints")
	}
}

// TestSystemSyslogNavPlacementAndGating pins Syslog into the System group
// after LLDP, and checks it's wired into the same sectionVisible
// capability-gating mechanism SNMP/LLDP/BGP already use, rather than
// being universally shown.
func TestSystemSyslogNavPlacementAndGating(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'lldp'", "'syslog'", "'users'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'lldp'") > strings.Index(block, "'syslog'") {
		t.Error("Syslog must come after LLDP in the System group")
	}
	if strings.Index(block, "'syslog'") > strings.Index(block, "'users'") {
		t.Error("Syslog must come before Users in the System group")
	}
	if !strings.Contains(indexHTML, "syslog:secSyslog") {
		t.Error("no syslog:secSyslog entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secSyslog(") {
		t.Error("secSyslog is not defined")
	}
	if !strings.Contains(indexHTML, "if (sec === 'syslog') return !!state.syslogSupported;") {
		t.Error("sectionVisible doesn't gate 'syslog' on state.syslogSupported")
	}
}

// TestSystemSyslogTableShape checks the page renders an actual manageable
// table (state/remote/port/protocol columns, add/remove wiring) rather
// than the old single-target form, so this doesn't regress back to one
// field with an enabled pill.
func TestSystemSyslogTableShape(t *testing.T) {
	fn := indexHTML[strings.Index(indexHTML, "function secSyslog("):]
	fn = fn[:strings.Index(fn, "\nasync function syslogReload(")]
	if !strings.Contains(fn, "syslogReload(") {
		t.Error("secSyslog doesn't kick off syslogReload")
	}
	reload := indexHTML[strings.Index(indexHTML, "async function syslogReload("):]
	reload = reload[:strings.Index(reload, "\nfunction syslogPayload(")]
	for _, want := range []string{"<th>state</th>", "<th>remote</th>", "<th>port</th>", "<th>protocol</th>", "_rowAdd", "_rowRemove"} {
		if !strings.Contains(reload, want) {
			t.Errorf("syslogReload is missing %q — expected a manageable table, not the old single-target form", want)
		}
	}
	if !strings.Contains(indexHTML, "function syslogAddRow(") {
		t.Error("syslogAddRow is not defined — the table should support adding multiple targets")
	}
}
