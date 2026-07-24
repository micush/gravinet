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

// TestSystemSyslogGet checks the read shape the page draws from. GET is
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
	for _, k := range []string{"enabled", "target", "protocol", "manager", "supported", "hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if enabled, _ := out["enabled"].(bool); enabled {
		t.Error("a fresh host should report enabled:false unless it already had a gravinet-managed forward configured")
	}
}

// TestSystemSyslogRejectsEnabledWithoutTarget is the one POST case this
// suite covers unconditionally, deliberately: validation
// (service.validSyslogTarget) runs before setLinuxSyslog/setBSDSyslog ever
// touch a real file or run a real command, so this is safe regardless of
// what's installed on the machine running the test suite — the same
// reasoning TestSystemSNMPRejectsEnabledWithoutCommunity's own doc comment
// gives for why this suite stops there rather than exercising a real save.
func TestSystemSyslogRejectsEnabledWithoutTarget(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"enabled": true, "target": "", "protocol": "udp"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/syslog", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("enabled:true with an empty target must be rejected")
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["error"].(string); !ok {
		t.Errorf("reply = %#v, want an \"error\" field", out)
	}
}

// TestSystemSyslogRejectsBadProtocol is the other request shape refused by
// validation before anything real is touched — same safety reasoning as
// TestSystemSyslogRejectsEnabledWithoutTarget above.
func TestSystemSyslogRejectsBadProtocol(t *testing.T) {
	ts, c := syslogTestServer(t)
	body, _ := json.Marshal(map[string]any{"enabled": true, "target": "log.example.com:514", "protocol": "quic"})
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

// TestSystemSyslogIsProxyable guards the placement decision: like
// Power/Time/Resolver/SNMP/L2Disco/Users, this endpoint follows the
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
// after L2 Disco, and checks it's wired into the same sectionVisible
// capability-gating mechanism SNMP/L2Disco/BGP already use, rather than
// being universally shown.
func TestSystemSyslogNavPlacementAndGating(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'l2disco'", "'syslog'", "'users'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'l2disco'") > strings.Index(block, "'syslog'") {
		t.Error("Syslog must come after L2 Disco in the System group")
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
