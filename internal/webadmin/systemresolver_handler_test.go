package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// TestSystemResolverGet checks the read shape secResolver draws from. Reuses
// timeTestServer (systemtime_handler_test.go) — /api/system/resolver needs no
// network config either, same reasoning as Time: it reads host state, not
// gravinet's config (see hostresolver.go's package doc).
func TestSystemResolverGet(t *testing.T) {
	ts, c := timeTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/resolver", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/resolver = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"hostname", "servers", "search_domain", "manager", "can_hostname", "can_dns", "hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if _, ok := out["servers"].([]any); !ok {
		t.Errorf("servers = %#v, want a JSON array even when empty", out["servers"])
	}
	if h, _ := out["hostname"].(string); h == "" {
		t.Error("hostname should reflect the real os.Hostname() of the test host, never empty")
	}
}

// TestSystemResolverRejectsBadRequests covers the refusal paths. Nothing here
// reaches an OS command — every case is caught by validation before dispatch —
// so running the suite can't change the test machine's hostname or DNS config.
func TestSystemResolverRejectsBadRequests(t *testing.T) {
	ts, c := timeTestServer(t)
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/resolver", bytes.NewReader(b))
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
		{"unknown op", map[string]any{"op": "nonsense"}, "op must be"},
		{"missing op", map[string]any{}, "op must be"},
		{"empty hostname", map[string]any{"op": "hostname", "hostname": ""}, "empty"},
		{"hostname injection", map[string]any{"op": "hostname", "hostname": "host; reboot"}, "invalid hostname"},
		{"hostname whitespace", map[string]any{"op": "hostname", "hostname": "bad host"}, "invalid hostname"},
		{"dns server not an IP", map[string]any{"op": "dns", "servers": []string{"dns.google"}}, "invalid DNS server"},
		{"dns server injection", map[string]any{"op": "dns", "servers": []string{"1.1.1.1", "evil; reboot"}}, "invalid DNS server"},
		{"search domain injection", map[string]any{"op": "dns", "servers": []string{"1.1.1.1"}, "search_domain": "bad domain; reboot"}, "invalid search domain"},
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

// TestSystemResolverIsProxyable guards the placement decision: like Power,
// Time, and Users, this endpoint follows the selected node, so it must NOT be
// pinned in the client's LOCAL_API list. Getting this wrong would silently
// apply a hostname/DNS change to the wrong host.
func TestSystemResolverIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/resolver") {
		t.Error("/api/system/resolver is pinned in LOCAL_API; it should follow the selected node like /api/system/time")
	}
}

// TestSystemResolverNavPlacement pins Resolver into System, directly above
// Time — matching parapet's own item order (resolver, time, dhcp, snmp,
// users, power), with Upgrade ahead of it (gravinet-only) and Power still
// last (the group's most destructive item; new items land above it).
func TestSystemResolverNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'upgrade'", "'resolver'", "'time'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	idx := func(item string) int { return strings.Index(block, "'"+item+"'") }
	if !(idx("upgrade") < idx("resolver") && idx("resolver") < idx("time") && idx("time") < idx("users") && idx("users") < idx("power")) {
		t.Errorf("System nav order must be upgrade, resolver, time, users, power; got positions %v",
			map[string]int{"upgrade": idx("upgrade"), "resolver": idx("resolver"), "time": idx("time"), "users": idx("users"), "power": idx("power")})
	}

	infoBlock := indexHTML[strings.Index(indexHTML, "{ name:'info'"):]
	infoBlock = infoBlock[:strings.Index(infoBlock, "]},")]
	if strings.Contains(infoBlock, "'resolver'") {
		t.Error("resolver leaked into the Info group")
	}

	if !strings.Contains(indexHTML, "resolver:secResolver") {
		t.Error("no resolver:secResolver entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secResolver(") {
		t.Error("secResolver is not defined")
	}
}

// resolverHandlerServer is a Server behind a real HTTP listener with a real
// config file, and it restores the indirected service calls afterwards so one
// test's stubs cannot leak into another's.
func resolverHandlerServer(t *testing.T) (*Server, *httptest.Server, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	srv := New(config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}},
		&stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	sh, sd, cr, rs := setHostnameFn, setHostDNSFn, canRestartFn, restartFn
	t.Cleanup(func() { setHostnameFn, setHostDNSFn, canRestartFn, restartFn = sh, sd, cr, rs })

	return srv, ts, sessionFor(t, ts)
}

func postResolver(t *testing.T, ts *httptest.Server, c *http.Cookie, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/resolver", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// hostnameInHistory reports whether any snapshot records the given hostname —
// the question a restore actually asks, since a restore reads a snapshot and
// not config.json.
func hostnameInHistory(t *testing.T, srv *Server, want string) bool {
	t.Helper()
	metas, err := config.List(srv.configPath)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	cur, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		_, cfg, err := config.Get(srv.configPath, m.ID, cur)
		if err != nil {
			continue
		}
		if cfg.HostSettings != nil && cfg.HostSettings.Resolver != nil &&
			cfg.HostSettings.Resolver.Hostname == want {
			return true
		}
	}
	return false
}

// A hostname change has to be committed to gravinet's config before anything
// schedules a restart, and this drives the real handler to check it.
//
// The hostname op is the only host-setting handler that restarts gravinet
// itself, and it flushes the pending history snapshot first so a snapshot
// mid-debounce is not lost with the process. That flush can only capture what
// is already committed. With the record happening after it, the flush found
// nothing pending, the record then started a fresh 3-second debounce, and the
// restart killed the process ~700ms later — so the change reached config.json
// and *no snapshot at all*. It survived a restart and was absent from every
// restore, which is the worst shape this can fail in: the setting looks right
// on the page and on the host, and only a restore reveals it was never written
// anywhere a restore reads.
//
// Asserted by effect, immediately after the reply: by then the handler's own
// flush must already have produced the snapshot, with no debounce waited out,
// because in production nothing waits either.
func TestHostnameIsRecordedBeforeRestartFlush(t *testing.T) {
	srv, ts, c := resolverHandlerServer(t)

	restarted := make(chan struct{}, 1)
	setHostnameFn = func(string) (bool, string) { return true, "" }
	canRestartFn = func() (bool, string) { return true, "" }
	restartFn = func() (bool, string) { restarted <- struct{}{}; return true, "" }

	if code := postResolver(t, ts, c, map[string]any{"op": "hostname", "hostname": "node7.example"}); code != 200 {
		t.Fatalf("POST op=hostname = %d, want 200", code)
	}

	if !hostnameInHistory(t, srv, "node7.example") {
		t.Error("the reply came back with no snapshot recording the hostname — " +
			"the restart is already scheduled, so a restore cannot bring it back")
	}
	cfg, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostSettings == nil || cfg.HostSettings.Resolver == nil ||
		cfg.HostSettings.Resolver.Hostname != "node7.example" {
		t.Errorf("config.json does not carry the hostname: %+v", cfg.HostSettings)
	}

	// The restart really was scheduled, so the flush above was load-bearing
	// rather than incidentally unnecessary in this test.
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Error("no restart was scheduled; this test is not covering the case it describes")
	}
}

// The DNS op shares the handler but never restarts, so its record is picked up
// by the ordinary debounce. Flushing explicitly here stands in for waiting the
// window out.
func TestResolverDNSIsRecorded(t *testing.T) {
	srv, ts, c := resolverHandlerServer(t)
	setHostDNSFn = func([]string, string) (bool, string) { return true, "" }
	canRestartFn = func() (bool, string) { return false, "not under a service manager" }

	if code := postResolver(t, ts, c, map[string]any{
		"op": "dns", "servers": []string{"10.0.0.53"}, "search_domain": "internal",
	}); code != 200 {
		t.Fatalf("POST op=dns = %d, want 200", code)
	}
	srv.flushPendingHistorySnapshot() // stands in for waiting out the debounce

	cfg, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	rz := cfg.HostSettings.Resolver
	if len(rz.DNSServers) != 1 || rz.DNSServers[0] != "10.0.0.53" || rz.SearchDomain != "internal" {
		t.Errorf("resolver not recorded as given: %+v", rz)
	}
}
