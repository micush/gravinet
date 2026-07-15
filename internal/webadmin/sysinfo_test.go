package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

func TestParseProcRoutes4(t *testing.T) {
	// Header line, then: default via 192.168.1.1 on eth0 (metric 100), and the
	// on-link 192.168.1.0/24. Destination/Gateway/Mask are little-endian hex.
	// 192.168.1.1 -> 0101A8C0; 192.168.1.0 -> 0001A8C0; mask /24 -> 00FFFFFF.
	const data = "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
	rows := parseProcRoutes4(strings.NewReader(data))
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Dest != "default" || rows[0].Gateway != "192.168.1.1" || rows[0].Iface != "eth0" || rows[0].Metric != 100 || rows[0].Family != 4 {
		t.Fatalf("default route wrong: %+v", rows[0])
	}
	if rows[1].Dest != "192.168.1.0/24" || rows[1].Gateway != "" {
		t.Fatalf("on-link route wrong: %+v", rows[1])
	}
}

func TestParseProcRoutes6(t *testing.T) {
	// fd00::/64 on-link via gravinet0, metric 0x400. dest = fd00 followed by zeros.
	dest := "fd00" + strings.Repeat("0", 28)
	zero := strings.Repeat("0", 32)
	line := dest + " 40 " + zero + " 00 " + zero + " 00000400 00000000 00000000 00000001 gravinet0\n"
	rows := parseProcRoutes6(strings.NewReader(line))
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Dest != "fd00::/64" || rows[0].Iface != "gravinet0" || rows[0].Metric != 0x400 || rows[0].Family != 6 {
		t.Fatalf("v6 route wrong: %+v", rows[0])
	}
}

func TestHexAndMaskHelpers(t *testing.T) {
	if got := hexToIP4("0101A8C0"); got.String() != "192.168.1.1" {
		t.Fatalf("hexToIP4 = %s", got)
	}
	for _, tc := range []struct {
		hex  string
		bits int
	}{{"00FFFFFF", 24}, {"0000FFFF", 16}, {"000000FF", 8}, {"FFFFFFFF", 32}, {"00000000", 0}} {
		if got := maskBits(hexToIP4(tc.hex)); got != tc.bits {
			t.Errorf("maskBits(%s) = %d, want %d", tc.hex, got, tc.bits)
		}
	}
}

func infoTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
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
	srv.SetVersion("9.9.9-test", "abc123")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

func getJSON(t *testing.T, ts *httptest.Server, c *http.Cookie, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, bytes.NewReader(nil))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestHandleAbout(t *testing.T) {
	ts, c := infoTestServer(t)
	b := getJSON(t, ts, c, "/api/about")
	if b["gravinet_version"] != "9.9.9-test" || b["gravinet_commit"] != "abc123" {
		t.Fatalf("version/commit not surfaced: %+v", b)
	}
	if b["os"] != runtime.GOOS || b["arch"] != runtime.GOARCH {
		t.Fatalf("os/arch wrong: %+v", b)
	}
	if b["go_version"] == "" || b["os_version"] == "" {
		t.Fatalf("expected non-empty go_version/os_version: %+v", b)
	}
}

// TestRouteCommandForOS locks in the per-OS native-route-listing command,
// including the one a live report caught missing: openbsd fell through to
// the default case (no command at all), so Monitor → Route Table silently
// rendered empty there — no error, nothing to distinguish it from "genuinely
// no routes". darwin/freebsd/openbsd all share netstat -rn (they're all BSD-
// family); windows uses route print; anything else (including linux, which
// never reaches this function — see handleLocalRoutes' own /proc/net/route
// path) gets no command and an empty, non-erroring result.
func TestRouteCommandForOS(t *testing.T) {
	cases := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{"darwin", "netstat", []string{"-rn"}},
		{"freebsd", "netstat", []string{"-rn"}},
		{"openbsd", "netstat", []string{"-rn"}},
		{"windows", "route", []string{"print"}},
		{"linux", "", nil},
		{"plan9", "", nil},
	}
	for _, tc := range cases {
		name, args := routeCommandForOS(tc.goos)
		if name != tc.wantName || !reflect.DeepEqual(args, tc.wantArgs) {
			t.Errorf("routeCommandForOS(%q) = (%q, %v), want (%q, %v)", tc.goos, name, args, tc.wantName, tc.wantArgs)
		}
	}
}

func TestHandleLocalRoutesAndHosts(t *testing.T) {
	ts, c := infoTestServer(t)
	// Routes: on Linux (the test environment) we expect structured entries.
	rb := getJSON(t, ts, c, "/api/localroutes")
	if runtime.GOOS == "linux" {
		ent, ok := rb["entries"].([]any)
		if !ok || len(ent) == 0 {
			t.Fatalf("expected structured routes on linux, got %+v", rb)
		}
	}
	// Hosts: the file should be readable and carry a path.
	hb := getJSON(t, ts, c, "/api/localhosts")
	if hb["path"] == "" {
		t.Fatalf("expected a hosts path: %+v", hb)
	}
	if _, ok := hb["text"].(string); !ok {
		t.Fatalf("expected hosts text: %+v", hb)
	}
}

// TestHandleLocalDNS verifies the Info → DNS handler's response shape and that
// it uses Backend.Interfaces() for the name/iface, tolerant of whether
// resolvectl (or the equivalent) actually exists in the test environment —
// either a live dump or a clean error is acceptable, but never neither.
func TestHandleLocalDNS(t *testing.T) {
	ts, c := infoTestServer(t)
	b := getJSON(t, ts, c, "/api/localdns")
	nets, ok := b["networks"].([]any)
	if !ok || len(nets) != 1 {
		t.Fatalf("expected exactly 1 network (from stubBackend.Interfaces), got %+v", b)
	}
	n, ok := nets[0].(map[string]any)
	if !ok {
		t.Fatalf("network entry wrong shape: %+v", nets[0])
	}
	if n["name"] != "lan" || n["iface"] != "mesh0" {
		t.Fatalf("name/iface should come from Interfaces(): %+v", n)
	}
	text, _ := n["text"].(string)
	errStr, _ := n["error"].(string)
	if text == "" && errStr == "" {
		t.Fatalf("expected either text or error to be populated: %+v", n)
	}
}

// TestHandleLocalLatency verifies the response shape and that it uses
// Backend.Interfaces()/ListPeers for network and peer identity. It's tolerant
// of whether the ping binary exists or 10.0.0.9 is reachable in the test
// environment — either result is fine, but the peer must always be present
// with a name/overlay, and ok/error must be consistent (never both empty).
func TestHandleLocalLatency(t *testing.T) {
	ts, c := infoTestServer(t)
	b := getJSON(t, ts, c, "/api/latency")
	nets, ok := b["networks"].([]any)
	if !ok || len(nets) != 1 {
		t.Fatalf("expected exactly 1 network (from stubBackend.Interfaces), got %+v", b)
	}
	n, ok := nets[0].(map[string]any)
	if !ok || n["name"] != "lan" {
		t.Fatalf("network entry wrong: %+v", nets[0])
	}
	peers, ok := n["peers"].([]any)
	if !ok || len(peers) != 1 {
		t.Fatalf("expected exactly 1 peer (from stubBackend.ListPeers), got %+v", n)
	}
	p, ok := peers[0].(map[string]any)
	if !ok || p["hostname"] != "hostx" || p["overlay"] != "10.0.0.9" {
		t.Fatalf("peer identity should come from ListPeers: %+v", p)
	}
	isOK, _ := p["ok"].(bool)
	errStr, _ := p["error"].(string)
	if !isOK && errStr == "" {
		t.Fatalf("a not-ok result must explain why: %+v", p)
	}
	if isOK {
		if rtt, ok := p["rtt_ms"].(float64); !ok || rtt <= 0 {
			t.Fatalf("an ok result must carry a positive rtt_ms: %+v", p)
		}
	}
}

// TestPingRTT checks the parser directly against a peer address that cannot
// possibly reply (TEST-NET-1, RFC 5737) — this should reliably produce a
// clean not-ok result (either "no reply" or a could-not-run error) rather
// than hanging or panicking, regardless of whether ping is installed here.
func TestPingRTT(t *testing.T) {
	_, err := pingRTT("192.0.2.1")
	if err == nil {
		t.Fatal("pinging a TEST-NET-1 address should not succeed")
	}
}

// TestPingArgsForOS locks in the per-OS ping(1) flags, independent of the
// test binary's own runtime.GOOS. Regression test for openbsd: it used to
// fall through to the Linux/default case and receive "-W 1", a flag OpenBSD's
// ping does not have, so every probe errored out before sending a packet and
// Info -> Latency reported "no reply" for every peer unconditionally.
func TestPingArgsForOS(t *testing.T) {
	cases := []struct {
		goos     string
		wantArgs []string
	}{
		{"windows", []string{"-n", "2", "-w", "1000", "10.0.0.1"}},
		{"darwin", []string{"-c", "2", "-t", "1", "10.0.0.1"}},
		{"freebsd", []string{"-c", "2", "-t", "1", "10.0.0.1"}},
		{"openbsd", []string{"-c", "2", "-w", "1", "10.0.0.1"}},
		{"linux", []string{"-c", "2", "-W", "1", "10.0.0.1"}},
		{"plan9", []string{"-c", "2", "-W", "1", "10.0.0.1"}},
	}
	for _, tc := range cases {
		args := pingArgsForOS(tc.goos, "10.0.0.1")
		if !reflect.DeepEqual(args, tc.wantArgs) {
			t.Errorf("pingArgsForOS(%q) = %v, want %v", tc.goos, args, tc.wantArgs)
		}
	}
	// The openbsd case must never use -W (capital), which is not a valid
	// OpenBSD ping flag and is what caused the original bug.
	for _, a := range pingArgsForOS("openbsd", "10.0.0.1") {
		if a == "-W" {
			t.Fatal("openbsd ping args must not include -W; OpenBSD's ping has no such flag")
		}
	}
}

// TestPingTimeRegexp locks in the parser against real ping output shapes from
// each platform this ships on, so a future refactor can't silently break
// parsing for one OS without a test catching it.
func TestPingTimeRegexp(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"64 bytes from 10.0.0.9: icmp_seq=1 ttl=64 time=0.042 ms", "0.042"}, // linux
		{"64 bytes from 10.0.0.9: icmp_seq=0 ttl=64 time=1.234 ms", "1.234"}, // darwin
		{"Reply from 10.0.0.9: bytes=32 time=2ms TTL=64", "2"},               // windows
		{"Reply from 10.0.0.9: bytes=32 time<1ms TTL=64", "1"},               // windows, sub-ms
	}
	for _, tc := range cases {
		m := pingTimeRe.FindStringSubmatch(tc.line)
		if len(m) != 2 || m[1] != tc.want {
			t.Errorf("parsing %q: got %v, want %q", tc.line, m, tc.want)
		}
	}
}
