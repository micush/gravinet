package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/service"
)

// TestConfigExposesSNMPAndL2DiscoSupported checks that /api/config carries
// the capability flags the web UI's sectionVisible() gates System > SNMP,
// System > L2 Disco, and System > Syslog on — the same "menu the user sees
// and the endpoint that backs it can never disagree" property bgp_supported
// already has (see bgpSupported's own doc comment). Compared against the
// live service.SNMPSupported/LLDPSupported/SyslogSupported results rather
// than a hardcoded true/false, since whether snmpd/lldpd/a syslog daemon
// are actually installed depends on whatever machine happens to be running
// this test suite.
func TestConfigExposesSNMPAndL2DiscoSupported(t *testing.T) {
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
	c := sessionFor(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/api/config", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/config = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	wantSNMP, _ := service.SNMPSupported()
	if got, ok := out["snmp_supported"].(bool); !ok || got != wantSNMP {
		t.Errorf("snmp_supported = %#v, want %v", out["snmp_supported"], wantSNMP)
	}
	wantL2Disco, _ := service.LLDPSupported()
	if got, ok := out["l2disco_supported"].(bool); !ok || got != wantL2Disco {
		t.Errorf("l2disco_supported = %#v, want %v", out["l2disco_supported"], wantL2Disco)
	}
	wantSyslog, _ := service.SyslogSupported()
	if got, ok := out["syslog_supported"].(bool); !ok || got != wantSyslog {
		t.Errorf("syslog_supported = %#v, want %v", out["syslog_supported"], wantSyslog)
	}
}
