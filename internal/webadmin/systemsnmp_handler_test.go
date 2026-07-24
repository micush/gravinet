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

// snmpTestServer stands up an authenticated admin server for the System >
// SNMP endpoint. Mirrors usersTestServer's/timeTestServer's shape.
func snmpTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
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

// TestSystemSNMPGet checks the read shape the page draws from. GET is
// read-only all the way down — service.SNMPServiceRunning does run a real
// `systemctl is-active`-style status *query* against whatever "snmpd"/"snmp"
// unit name resolves on this host, but a status query can't mutate
// anything, so this is safe regardless of whether that unit happens to
// exist on the machine running the test suite (unlike a POST that actually
// enables/disables one — see TestSystemSNMPRejectsEnabledWithoutCommunity's
// doc comment for why this suite stops there).
func TestSystemSNMPGet(t *testing.T) {
	ts, c := snmpTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/snmp", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/snmp = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"enabled", "community", "listen_addr", "interfaces", "location", "contact", "running", "supported", "hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if enabled, _ := out["enabled"].(bool); enabled {
		t.Error("a fresh config should report enabled:false")
	}
	if _, ok := out["interfaces"].([]any); !ok {
		t.Errorf("interfaces = %#v, want a JSON array even when empty", out["interfaces"])
	}
}

// TestSystemSNMPRejectsEnabledWithoutCommunity is the one POST case this
// suite covers, deliberately: it's the only request shape guaranteed to be
// refused by validation *before* the handler ever reaches
// service.ApplySNMP, which — for any request that gets past validation —
// goes on to actually enable/disable or start/stop the real snmpd service
// via systemctl/service/rcctl/brew services. A successful save is not
// tested here for exactly that reason: on a machine where a "snmpd" or
// "snmp" unit happens to already exist, exercising that path for real would
// toggle a real system service as a side effect of running this test suite
// — the same risk sysusers_test.go/groups_test.go already refuse to take
// for useradd/groupadd, applied here to systemctl instead.
func TestSystemSNMPRejectsEnabledWithoutCommunity(t *testing.T) {
	ts, c := snmpTestServer(t)
	body, _ := json.Marshal(map[string]any{"enabled": true, "community": ""})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/snmp", strings.NewReader(string(body)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("enabled:true with an empty community must be rejected")
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "community") {
		t.Errorf("error = %q, want it to mention the missing community string", msg)
	}
}

// TestSystemSNMPIsProxyable guards the placement decision: like
// Power/Time/Users, this endpoint follows the selected node, so it must NOT
// be in the client's LOCAL_API pin-to-this-node list.
func TestSystemSNMPIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/snmp") {
		t.Error("/api/system/snmp is pinned in LOCAL_API; it should follow the selected node like the other System > * endpoints")
	}
}

// TestSystemSNMPNavPlacement pins SNMP into the System group between Time
// and Users, matching parapet's own relative ordering (resolver, time,
// snmp, users, power there too, minus the dhcp item gravinet dropped).
func TestSystemSNMPNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'time'", "'snmp'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'time'") > strings.Index(block, "'snmp'") {
		t.Error("SNMP must come after Time in the System group")
	}
	if strings.Index(block, "'snmp'") > strings.Index(block, "'users'") {
		t.Error("SNMP must come before Users in the System group")
	}
	if !strings.Contains(indexHTML, "snmp:secSNMP") {
		t.Error("no snmp:secSNMP entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secSNMP(") {
		t.Error("secSNMP is not defined")
	}
}
