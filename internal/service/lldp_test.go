package service

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gravinet/internal/config"
)

func TestLLDPValidIface(t *testing.T) {
	valid := []string{"eth0", "enp0s3", "bond0.100", "br-lan", "eth0_1", "eth0@if2"}
	for _, s := range valid {
		if !ValidLLDPIface(s) {
			t.Errorf("ValidLLDPIface(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "eth0; rm -rf /", "eth 0", strings.Repeat("a", 16), "eth0\n"}
	for _, s := range invalid {
		if ValidLLDPIface(s) {
			t.Errorf("ValidLLDPIface(%q) = true, want false", s)
		}
	}
}

func TestLLDPConfigIsRunnableAndAnyCDP(t *testing.T) {
	var d config.DiscoveryConfig
	if d.IsRunnable() {
		t.Error("empty config should not be runnable")
	}
	d.Interfaces = []config.DiscoveryIface{{Name: "lo", LLDP: true, CDP: true}}
	if d.IsRunnable() {
		t.Error("loopback-only, even with both protocols on, should not be runnable")
	}
	if d.AnyCDP() {
		t.Error("loopback-only CDP should not count")
	}
	d.Interfaces = append(d.Interfaces, config.DiscoveryIface{Name: "eth0", LLDP: false, CDP: false})
	if d.IsRunnable() {
		t.Error("an interface with both protocols off should not make the config runnable")
	}
	d.Interfaces[1].LLDP = true
	if !d.IsRunnable() {
		t.Error("eth0 with LLDP on should make the config runnable")
	}
	if d.AnyCDP() {
		t.Error("no interface has CDP on yet")
	}
	d.Interfaces[1].CDP = true
	if !d.AnyCDP() {
		t.Error("eth0 now has CDP on")
	}
}

func TestLLDPArgsBuildsExpectedArgv(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.DiscoveryConfig
		want []string
	}{
		{"lldp only, one iface", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0", LLDP: true},
		}}, []string{"-d", "-I", "eth0"}},
		{"lldp+cdp, one iface", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0", LLDP: true, CDP: true},
		}}, []string{"-d", "-c", "-I", "eth0"}},
		{"cdp only counts as active too", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth1", CDP: true},
		}}, []string{"-d", "-c", "-I", "eth1"}},
		{"loopback excluded even if flagged", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "lo", LLDP: true, CDP: true},
			{Name: "eth0", LLDP: true},
		}}, []string{"-d", "-I", "eth0"}}, // no -c: lo's CDP must not count
		{"invalid iface name dropped, not injected", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0; rm -rf /", LLDP: true},
			{Name: "eth1", LLDP: true},
		}}, []string{"-d", "-I", "eth1"}},
		{"nothing active", config.DiscoveryConfig{}, []string{"-d"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lldpArgs(c.cfg)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("lldpArgs() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLLDPArgsMultipleIfacesJoinedWithCommaNotSpace(t *testing.T) {
	cfg := config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
		{Name: "eth0", LLDP: true},
		{Name: "eth1", LLDP: true},
	}}
	args := lldpArgs(cfg)
	// -I's value is a single argv token, comma-joined — never two separate
	// tokens, which would silently change lldpd's -I parsing.
	idx := -1
	for i, a := range args {
		if a == "-I" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("no -I flag found in %v", args)
	}
	val := args[idx+1]
	parts := strings.Split(val, ",")
	sort.Strings(parts)
	if strings.Join(parts, ",") != "eth0,eth1" {
		t.Errorf("-I value = %q, want a comma-joined \"eth0,eth1\" (in either order)", val)
	}
}

// TestParseLLDPNeighborsObjectShape covers lldpd's "interface as object"
// JSON shape, ported from a representative real lldpcli -f json output.
func TestParseLLDPNeighborsObjectShape(t *testing.T) {
	data := []byte(`{
		"lldp": {
			"interface": {
				"eth0": {
					"chassis": {
						"switch1.example": {
							"id": {"type": "mac", "value": "aa:bb:cc:dd:ee:ff"},
							"mgmt-ip": "10.0.0.1"
						}
					},
					"port": {
						"descr": "GigabitEthernet0/1"
					}
				}
			}
		}
	}`)
	rows, err := parseLLDPNeighborsJSON(data)
	if err != nil {
		t.Fatalf("parseLLDPNeighborsJSON: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.LocalIface != "eth0" {
		t.Errorf("LocalIface = %q, want eth0", r.LocalIface)
	}
	if r.SystemName != "switch1.example" {
		t.Errorf("SystemName = %q, want switch1.example", r.SystemName)
	}
	if r.Port != "GigabitEthernet0/1" {
		t.Errorf("Port = %q, want GigabitEthernet0/1", r.Port)
	}
	if r.MgmtIP != "10.0.0.1" {
		t.Errorf("MgmtIP = %q, want 10.0.0.1", r.MgmtIP)
	}
}

// TestParseLLDPNeighborsArrayShape covers the alternate "interface as
// array of single-key objects" shape parapet's own comment says some lldpd
// versions use instead.
func TestParseLLDPNeighborsArrayShape(t *testing.T) {
	data := []byte(`{
		"lldp": {
			"interface": [
				{"eth0": {
					"chassis": {"core-sw": {"mgmt-ip": "192.168.1.1"}},
					"port": {"id": {"type": "ifname", "value": "eth3"}}
				}},
				{"eth1": {
					"chassis": {"edge-sw": {}},
					"port": {}
				}}
			]
		}
	}`)
	rows, err := parseLLDPNeighborsJSON(data)
	if err != nil {
		t.Fatalf("parseLLDPNeighborsJSON: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byIface := map[string]LLDPNeighbor{}
	for _, r := range rows {
		byIface[r.LocalIface] = r
	}
	eth0, ok := byIface["eth0"]
	if !ok {
		t.Fatal("missing eth0 row")
	}
	if eth0.SystemName != "core-sw" || eth0.Port != "eth3" || eth0.MgmtIP != "192.168.1.1" {
		t.Errorf("eth0 row = %+v, want system=core-sw port=eth3 mgmt=192.168.1.1", eth0)
	}
	eth1, ok := byIface["eth1"]
	if !ok {
		t.Fatal("missing eth1 row")
	}
	// port has neither descr nor id.value, and chassis's inner object has no
	// recognized name field either — falls back to the sole map key, "edge-sw".
	if eth1.SystemName != "edge-sw" {
		t.Errorf("eth1 SystemName = %q, want edge-sw (fallback to sole chassis key)", eth1.SystemName)
	}
}

func TestParseLLDPNeighborsEmptyAndMalformed(t *testing.T) {
	rows, err := parseLLDPNeighborsJSON([]byte(`{"lldp": {}}`))
	if err != nil {
		t.Fatalf("unexpected error on empty lldp object: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %+v", rows)
	}
	if _, err := parseLLDPNeighborsJSON([]byte(`not json`)); err == nil {
		t.Error("expected an error parsing non-JSON input")
	}
}

// TestLLDPCrashHint pins the log-line -> hint mapping this bug report
// motivated adding: without it, a failed start only ever reported systemd's
// own generic "control process exited with error code," with no way to
// tell an SELinux denial from a stale socket from a genuine config problem.
func TestLLDPCrashHint(t *testing.T) {
	cases := []struct {
		line       string
		wantSubstr string // "" means no hint expected
	}{
		{`lldpd[1234]: fatal: avc: denied { write } for pid=1234 comm="lldpd"`, "SELinux"},
		{`SELinux is preventing lldpd from ...`, "SELinux"},
		{`Permission denied while opening /var/run/lldpd.socket`, "SELinux"},
		{`apparmor="DENIED" operation="open" profile="lldpd"`, "AppArmor"},
		{`lldpd: another instance is running, giving up`, "already be present"},
		{`bind: Address already in use`, "already be present"},
		{`lldpd: unrecognized option '--bogus'`, ""},
		{`lldpd: started successfully`, ""},
	}
	for _, c := range cases {
		got := lldpCrashHint(c.line)
		if c.wantSubstr == "" {
			if got != "" {
				t.Errorf("lldpCrashHint(%q) = %q, want no hint", c.line, got)
			}
			continue
		}
		if !strings.Contains(got, c.wantSubstr) {
			t.Errorf("lldpCrashHint(%q) = %q, want it to mention %q", c.line, got, c.wantSubstr)
		}
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"\n\n\n", ""},
		{"one line", "one line"},
		{"first\nsecond\nthird", "third"},
		{"first\nsecond\n\n  \n", "second"},
		{"  padded with spaces  \n", "padded with spaces"},
	}
	for _, c := range cases {
		if got := lastNonEmptyLine(c.in); got != c.want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Truncation: a single very long line is capped, not returned whole.
	long := strings.Repeat("x", 500)
	got := lastNonEmptyLine(long)
	if len([]rune(got)) != 200 {
		t.Errorf("lastNonEmptyLine truncation: got length %d, want 200", len([]rune(got)))
	}
}

// TestLLDPStaleSocketPathsIncludesConfirmedPath pins the one candidate path
// that isn't a guess: /run/lldpd/lldpd.socket was named directly in a real
// SELinux AVC denial (source process lldpd, access "connectto") reported
// against a real gravinet install, confirming that some distros' lldpd
// really does put its control socket in a subdirectory — not just at
// /run/lldpd.socket the way parapet's own (Debian/Ubuntu-focused) cleanup
// assumed. If this ever gets refactored away, the exact failure this test
// suite exists to catch would silently come back.
func TestLLDPStaleSocketPathsIncludesConfirmedPath(t *testing.T) {
	found := false
	for _, p := range lldpStaleSocketPaths {
		if p == "/run/lldpd/lldpd.socket" {
			found = true
		}
	}
	if !found {
		t.Error("lldpStaleSocketPaths is missing /run/lldpd/lldpd.socket, the path directly confirmed via a real SELinux denial")
	}
}

// TestRemoveSocketsAt checks the cleanup actually removes what's there and
// is a silent no-op for what isn't — against a temp-dir fixture, never the
// real (root-owned) /run paths lldpStaleSocketPaths names.
func TestRemoveSocketsAt(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "lldpd.socket")
	if err := os.WriteFile(existing, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "does-not-exist.socket")
	subdirExisting := filepath.Join(dir, "sub", "lldpd.socket")
	if err := os.MkdirAll(filepath.Dir(subdirExisting), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subdirExisting, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Must not panic or error out just because "missing" isn't there.
	removeSocketsAt([]string{existing, missing, subdirExisting})

	if _, err := os.Stat(existing); !os.IsNotExist(err) {
		t.Errorf("%s should have been removed, stat err = %v", existing, err)
	}
	if _, err := os.Stat(subdirExisting); !os.IsNotExist(err) {
		t.Errorf("%s should have been removed, stat err = %v", subdirExisting, err)
	}
}
