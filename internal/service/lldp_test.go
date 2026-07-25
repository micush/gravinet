package service

import (
	"os"
	"path/filepath"
	"runtime"
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
					"via": "LLDP",
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
	if r.Protocol != "LLDP" {
		t.Errorf("Protocol = %q, want LLDP", r.Protocol)
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

// TestParseLLDPNeighborsProtocolCDPAndMixed covers what motivated adding
// Protocol in the first place: lldpd (started with -c whenever any
// interface has CDP on — see lldpArgs) reports CDP-discovered neighbors in
// exactly the same "show neighbors" JSON as LLDP ones, distinguished only
// by "via". A switch that speaks both protocols on the same port shows up
// as two separate array entries under the same interface name (the array
// shape, not the object shape, since a JSON object can't repeat a key) —
// this is the real-world case Monitor › L2 Peers needs to tell apart.
func TestParseLLDPNeighborsProtocolCDPAndMixed(t *testing.T) {
	data := []byte(`{
		"lldp": {
			"interface": [
				{"eth0": {
					"via": "LLDP",
					"chassis": {"switch1.example": {"mgmt-ip": "10.0.0.1"}},
					"port": {"descr": "Gi0/1"}
				}},
				{"eth0": {
					"via": "CDPv2",
					"chassis": {"switch1": {"mgmt-ip": "10.0.0.1"}},
					"port": {"descr": "GigabitEthernet0/1"}
				}},
				{"eth1": {
					"chassis": {"legacy-switch": {}},
					"port": {}
				}}
			]
		}
	}`)
	rows, err := parseLLDPNeighborsJSON(data)
	if err != nil {
		t.Fatalf("parseLLDPNeighborsJSON: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (both eth0 protocols plus eth1): %+v", len(rows), rows)
	}
	var lldpRow, cdpRow, noViaRow *LLDPNeighbor
	for i := range rows {
		switch {
		case rows[i].LocalIface == "eth0" && rows[i].Protocol == "LLDP":
			lldpRow = &rows[i]
		case rows[i].LocalIface == "eth0" && rows[i].Protocol == "CDPv2":
			cdpRow = &rows[i]
		case rows[i].LocalIface == "eth1":
			noViaRow = &rows[i]
		}
	}
	if lldpRow == nil {
		t.Fatal("missing eth0's LLDP-via row")
	}
	if cdpRow == nil {
		t.Fatal("missing eth0's CDPv2-via row")
	}
	// Both protocols saw the same switch on the same port; each is its own
	// row, not merged into one — merging would hide that the two protocols
	// independently confirm the same physical neighbor.
	if lldpRow.SystemName != "switch1.example" || cdpRow.SystemName != "switch1" {
		t.Errorf("eth0 rows = LLDP:%+v CDPv2:%+v, want distinct SystemName per protocol (LLDP and CDP name the same box differently in practice)", lldpRow, cdpRow)
	}
	if noViaRow == nil {
		t.Fatal("missing eth1 row")
	}
	if noViaRow.Protocol != "" {
		t.Errorf("eth1 Protocol = %q, want empty when lldpd's JSON omits \"via\" entirely", noViaRow.Protocol)
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

// TestParseLLDPProcs pins parsing against the exact `pgrep -a -x lldpd`
// output confirmed against a real host: a current instance (wlan0, no CDP)
// running as its privsep monitor+worker pair sharing one command line, plus
// an older leftover pair (eth0, CDP on) from a previous configuration.
// Every pid must survive parsing — they all get signalled — while dedup for
// *reporting* collapses each pair to one line.
func TestParseLLDPProcs(t *testing.T) {
	out := `24452 /usr/sbin/lldpd -d -c -I eth0
24454 /usr/sbin/lldpd -d -c -I eth0
1928776 /usr/sbin/lldpd -d -I wlan0
1928950 /usr/sbin/lldpd -d -I wlan0
`
	procs := parseLLDPProcs(out)
	if len(procs) != 4 {
		t.Fatalf("parseLLDPProcs(...) returned %d procs, want all 4 pids", len(procs))
	}
	wantPIDs := map[int]bool{24452: true, 24454: true, 1928776: true, 1928950: true}
	for _, p := range procs {
		if !wantPIDs[p.PID] {
			t.Errorf("unexpected pid %d", p.PID)
		}
		if !strings.HasPrefix(p.Argv, "/usr/sbin/lldpd ") {
			t.Errorf("pid %d argv = %q, want the full command line", p.PID, p.Argv)
		}
	}

	argvs := dedupLLDPArgvs(procs)
	want := []string{"/usr/sbin/lldpd -d -c -I eth0", "/usr/sbin/lldpd -d -I wlan0"}
	sort.Strings(argvs)
	sort.Strings(want)
	if len(argvs) != len(want) {
		t.Fatalf("dedupLLDPArgvs(...) = %v, want %v", argvs, want)
	}
	for i := range want {
		if argvs[i] != want[i] {
			t.Errorf("dedupLLDPArgvs(...)[%d] = %q, want %q", i, argvs[i], want[i])
		}
	}
}

// TestParseLLDPProcsRejectsUnusableLines checks the no-lldpd-running case
// and lines that can't safely be turned into a pid. pid 1 especially: every
// pid this returns gets signalled, so init must never come back out of it.
func TestParseLLDPProcsRejectsUnusableLines(t *testing.T) {
	for _, in := range []string{"", "\n  \n", "24452\n", "notapid /usr/sbin/lldpd -d\n", "1 /sbin/init\n", "0 /usr/sbin/lldpd -d\n"} {
		if got := parseLLDPProcs(in); len(got) != 0 {
			t.Errorf("parseLLDPProcs(%q) = %+v, want nothing usable", in, got)
		}
	}
}

// TestStrayLLDPProcsRules pins which processes are considered strays. The
// runnable case is linux-only on purpose (see strayLLDPProcs' own comment:
// on the BSDs the real argv contains base rc.d flags gravinet never wrote,
// so exact-matching there would terminate the correctly-running instance),
// which is exactly the false positive this guards against.
func TestStrayLLDPProcsRules(t *testing.T) {
	const bin = "/usr/sbin/lldpd"
	want := bin + " -d -I wlan0"
	current := lldpProc{PID: 1000, Argv: want}
	leftover := lldpProc{PID: 2000, Argv: bin + " -d -c -I eth0"}

	// Nothing running: never anything to reap, whatever the config says.
	if got := strayLLDPProcs(true, want, nil); len(got) != 0 {
		t.Errorf("no processes running, got strays %+v", got)
	}

	// Switched off — the reported screenshot's exact state: nothing should be
	// running at all, so everything is a stray, on every platform.
	got := strayLLDPProcs(false, "", []lldpProc{current, leftover})
	if len(got) != 2 {
		t.Errorf("discovery off: got %d strays, want both processes reaped: %+v", len(got), got)
	}

	// On: only the mismatched leftover, and only where gravinet owns the
	// whole command line.
	got = strayLLDPProcs(true, want, []lldpProc{current, leftover})
	if runtime.GOOS == "linux" {
		if len(got) != 1 || got[0].PID != leftover.PID {
			t.Errorf("got %+v, want only the leftover (pid %d)", got, leftover.PID)
		}
	} else if len(got) != 0 {
		t.Errorf("got %+v, want nothing: argv can't be compared reliably on %s", got, runtime.GOOS)
	}

	// No expected argv to compare against (lldpd not installed): must never
	// guess, or it would terminate a perfectly good running instance.
	if got := strayLLDPProcs(true, "", []lldpProc{current, leftover}); len(got) != 0 {
		t.Errorf("got %+v, want nothing when there's no argv to compare against", got)
	}
}

// TestExcludeOpenBSDBaseLLDPD guards the fix for a real risk the OpenBSD
// bug report exposed one layer down from lldpdBinary: strayLLDPProcs treats
// "discovery switched off" as "every lldpd-named process on the host is a
// stray" (see TestStrayLLDPProcsRules' own "switched off" case above) —
// which is fine when the only thing that could ever be named exactly
// "lldpd" is something gravinet itself started, but not once OpenBSD 7.8's
// unrelated base lldpd(8) can be running independently, for an operator's
// own reasons, while gravinet's own L2 Disco happens to be off (the
// default state). Without this filter, gravinet's once-at-startup reaper
// would kill that unrelated daemon outright.
func TestExcludeOpenBSDBaseLLDPD(t *testing.T) {
	base := lldpProc{PID: 100, Argv: "/usr/sbin/lldpd"}
	baseWithFlags := lldpProc{PID: 101, Argv: "/usr/sbin/lldpd -d -s /var/run/lldp.sock"}
	pkg := lldpProc{PID: 200, Argv: "/usr/local/sbin/lldpd -d -c -I em0"}
	lookalike := lldpProc{PID: 300, Argv: "/usr/sbin/lldpd-something-else -d"}

	got := excludeOpenBSDBaseLLDPD([]lldpProc{base, baseWithFlags, pkg, lookalike})
	if len(got) != 2 {
		t.Fatalf("excludeOpenBSDBaseLLDPD(...) = %+v, want exactly the pkg and lookalike procs left", got)
	}
	byPID := map[int]bool{}
	for _, p := range got {
		byPID[p.PID] = true
	}
	if !byPID[pkg.PID] {
		t.Error("the ports net/lldpd instance must survive filtering — it's a real gravinet-managed candidate")
	}
	if !byPID[lookalike.PID] {
		t.Error("a differently-named binary that merely starts with the same prefix must survive filtering")
	}
	if byPID[base.PID] || byPID[baseWithFlags.PID] {
		t.Errorf("OpenBSD's base lldpd(8) (bare or with flags) must be filtered out, got %+v", got)
	}

	if got := excludeOpenBSDBaseLLDPD(nil); len(got) != 0 {
		t.Errorf("excludeOpenBSDBaseLLDPD(nil) = %+v, want nothing", got)
	}
}

// TestOpenBSDLLDPServiceNameFrom pins the exact rename that produced the
// second half of the real bug report this fix addresses: "couldn't rcctl
// set lldpd flags: rcctl: service lldpd does not exist" persisted even
// after excluding the base binary (TestExcludeOpenBSDBaseLLDPD above),
// because on OpenBSD 7.9 the ports net/lldpd package's own rc.d(8) script
// — the one gravinet actually drives — was itself renamed from "lldpd" to
// "elldpd", confirmed on OpenBSD's own 7.8->7.9 upgrade guide ("to free up
// the rc script name for future use in base"). Hardcoding "lldpd" broke
// the moment that guide took effect; this checks the three states that
// matter instead.
func TestOpenBSDLLDPServiceNameFrom(t *testing.T) {
	cases := []struct {
		name                                  string
		lldpdScriptExists, elldpdScriptExists bool
		want                                  string
	}{
		{"pre-7.9: only the old lldpd script exists", true, false, "lldpd"},
		{"7.9+: only the renamed elldpd script exists", false, true, "elldpd"},
		{"both exist (hypothetical transition moment): prefer the unrenamed one", true, true, "lldpd"},
		{"neither exists (lldpd not installed): fall back to the recognizable name", false, false, "lldpd"},
	}
	for _, c := range cases {
		if got := openBSDLLDPServiceNameFrom(c.lldpdScriptExists, c.elldpdScriptExists); got != c.want {
			t.Errorf("%s: openBSDLLDPServiceNameFrom(%v, %v) = %q, want %q",
				c.name, c.lldpdScriptExists, c.elldpdScriptExists, got, c.want)
		}
	}
}
