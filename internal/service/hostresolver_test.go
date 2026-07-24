package service

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/tun"
)

// TestValidHostnameRejectsInjection is the important half of this file: every
// hostname that reaches SetHostname becomes an exec argument (hostnamectl,
// scutil's stdin, sysrc, Rename-Computer's -Command string) on some platform,
// and arrives from a browser field. Shell metacharacters, quotes, and
// whitespace must all be refused.
func TestValidHostnameRejectsInjection(t *testing.T) {
	bad := []string{
		"host; rm -rf /",
		"host && curl example.com",
		"host|sh",
		"$(id)",
		"`id`",
		"host name",   // whitespace
		"host'name",   // breaks out of PowerShell's single-quoted -NewName
		"-startswith", // leading hyphen: label rule, and looks like a flag
		"trailing-.example",
		strings.Repeat("a", 300),
	}
	for _, h := range bad {
		if err := validHostname(h); err == nil {
			t.Errorf("validHostname(%q) accepted a value it must reject", h)
		}
	}
}

func TestValidHostnameAcceptsRealNames(t *testing.T) {
	good := []string{"node7", "node-7", "a", "node7.example.com", "a.b.c.d"}
	for _, h := range good {
		if err := validHostname(h); err != nil {
			t.Errorf("validHostname(%q) rejected a legitimate hostname: %v", h, err)
		}
	}
	if err := validHostname(""); err == nil {
		t.Error("validHostname(\"\") should be rejected — SetHostname treats empty specially before ever calling this, but the function itself must not silently accept it")
	}
}

func TestValidLabelBoundaries(t *testing.T) {
	cases := []struct {
		label string
		want  bool
	}{
		{"a", true},
		{"a-b", true},
		{"-ab", false}, // leading hyphen
		{"ab-", false}, // trailing hyphen
		{"", false},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"a_b", false}, // underscore not allowed in a hostname label
	}
	for _, c := range cases {
		if got := validLabel(c.label); got != c.want {
			t.Errorf("validLabel(%q) = %v, want %v", c.label, got, c.want)
		}
	}
}

// TestValidSearchDomainAcceptsSingleLabel matters because a search domain
// (unlike most hostnames anyone would set) is routinely just one label, e.g.
// "internal" — and parapet's own valid_search_domain is defined as literally
// calling valid_hostname, so this must accept it the same way.
func TestValidSearchDomainAcceptsSingleLabel(t *testing.T) {
	for _, d := range []string{"internal", "corp.internal", "a"} {
		if err := validSearchDomain(d); err != nil {
			t.Errorf("validSearchDomain(%q) rejected a legitimate domain: %v", d, err)
		}
	}
	if err := validSearchDomain("bad domain"); err == nil {
		t.Error("validSearchDomain(\"bad domain\") accepted a value with whitespace")
	}
}

func TestValidDNSServerAddr(t *testing.T) {
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "::1", "127.0.0.1"} {
		if err := validDNSServerAddr(s); err != nil {
			t.Errorf("validDNSServerAddr(%q) rejected a real address: %v", s, err)
		}
	}
	// Unlike System > Time's NTP servers (legitimately hostnames most of the
	// time), a DNS server here MUST be a literal address — accepting a
	// hostname would be circular, since resolving it is the very question this
	// field answers. Also covers the same injection surface as validHostname.
	for _, s := range []string{"dns.google", "1.1.1.1; rm -rf /", "$(id)", "not-an-ip", "", "1.1.1.1 8.8.8.8"} {
		if err := validDNSServerAddr(s); err == nil {
			t.Errorf("validDNSServerAddr(%q) accepted a value it must reject", s)
		}
	}
}

func TestParseHardwarePorts(t *testing.T) {
	sample := "Hardware Port: Wi-Fi\n" +
		"Device: en0\n" +
		"Ethernet Address: aa:bb:cc:dd:ee:ff\n" +
		"\n" +
		"Hardware Port: Thunderbolt Ethernet\n" +
		"Device: en5\n" +
		"Ethernet Address: 11:22:33:44:55:66\n" +
		"\n" +
		"Hardware Port: Bluetooth PAN\n" +
		"Device: en7\n" +
		"Ethernet Address: 00:00:00:00:00:00\n"
	got := parseHardwarePorts(sample)
	want := map[string]string{"en0": "Wi-Fi", "en5": "Thunderbolt Ethernet", "en7": "Bluetooth PAN"}
	if len(got) != len(want) {
		t.Fatalf("parseHardwarePorts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseHardwarePorts[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestResolveServiceName(t *testing.T) {
	ports := map[string]string{"en0": "Wi-Fi"}
	svc, err := resolveServiceName("en0", ports)
	if err != nil || svc != "Wi-Fi" {
		t.Errorf("resolveServiceName(en0) = (%q, %v), want (Wi-Fi, nil)", svc, err)
	}
	// The case that matters most: the default route sitting on gravinet's own
	// tun interface (e.g. full-tunnel mode), which networksetup has never heard
	// of. Must fail with an explanatory error, not a lookup panic or a silent
	// empty string.
	_, err = resolveServiceName("utun7", ports)
	if err == nil {
		t.Error("resolveServiceName on an unknown interface should fail")
	}
	if !strings.Contains(err.Error(), "utun7") {
		t.Errorf("error %q should name the interface that didn't match", err)
	}
}

// TestDefaultServiceNameEndToEnd exercises defaultServiceName's actual glue
// (gateway lookup -> net.InterfaceByIndex -> hardwarePortsFn -> lookup) using
// the loopback interface, which is guaranteed present on every host this test
// runs on, so it needs no real default route or real networksetup.
func TestDefaultServiceNameEndToEnd(t *testing.T) {
	old, oldPorts := sysDefaultGatewayFn, hardwarePortsFn
	defer func() { sysDefaultGatewayFn, hardwarePortsFn = old, oldPorts }()

	loopback, err := findLoopback()
	if err != nil {
		t.Skipf("no loopback interface on this test host: %v", err)
	}
	sysDefaultGatewayFn = func(family int, exclude int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("127.0.0.1"), IfIndex: int32(loopback)}, nil
	}
	hardwarePortsFn = func() (map[string]string, error) {
		return map[string]string{loopbackName(t, loopback): "Loopback Test Service"}, nil
	}
	svc, err := defaultServiceName()
	if err != nil {
		t.Fatalf("defaultServiceName: %v", err)
	}
	if svc != "Loopback Test Service" {
		t.Errorf("defaultServiceName = %q, want %q", svc, "Loopback Test Service")
	}
}

func TestParseRootForward(t *testing.T) {
	sample := "example.internal. IN forward 10.0.0.1\n" +
		". IN forward 1.1.1.1 8.8.8.8\n" +
		"other.internal. IN forward 10.0.0.2\n"
	got := parseRootForward(sample)
	want := []string{"1.1.1.1", "8.8.8.8"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("parseRootForward = %v, want %v — must isolate the root zone's line from surrounding zones", got, want)
	}

	if got := parseRootForward("no forwards configured\n"); got != nil {
		t.Errorf("parseRootForward on empty output = %v, want nil", got)
	}
	// A zone line that isn't the root must never match.
	if got := parseRootForward("example.com. IN forward 9.9.9.9\n"); got != nil {
		t.Errorf("parseRootForward matched a non-root zone: %v", got)
	}
}

func TestResolvedConfSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gravinet-system-resolver.conf")
	body := "# Managed by [gravinet] (System > Resolver).\n[Resolve]\nDNS=1.1.1.1 8.8.8.8\nDomains=corp.internal\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	kv := resolvedConfSection(path)
	if kv["DNS"] != "1.1.1.1 8.8.8.8" || kv["Domains"] != "corp.internal" {
		t.Errorf("resolvedConfSection = %v", kv)
	}

	// A key outside [Resolve] must not leak in — guards against a naive
	// whole-file key=value scan picking up something from a different section.
	multi := "[Match]\nName=eth0\n[Resolve]\nDNS=9.9.9.9\n"
	path2 := filepath.Join(dir, "multi.conf")
	os.WriteFile(path2, []byte(multi), 0o644)
	kv2 := resolvedConfSection(path2)
	if _, ok := kv2["Name"]; ok {
		t.Error("resolvedConfSection picked up a key from outside [Resolve]")
	}
	if kv2["DNS"] != "9.9.9.9" {
		t.Errorf("resolvedConfSection missed DNS inside [Resolve]: %v", kv2)
	}

	if kv3 := resolvedConfSection(filepath.Join(dir, "missing.conf")); kv3 != nil {
		t.Errorf("resolvedConfSection on a missing file = %v, want nil", kv3)
	}
}

// TestUnixDirectResolvConf covers the fallback path's full lifecycle: write,
// re-write, and the marker-gated clear. Redirects the package-level
// resolvConfPath var to a temp file so this never touches the real
// /etc/resolv.conf, the same technique timesyncdConfPath uses in
// hosttime_test.go.
func TestUnixDirectResolvConf(t *testing.T) {
	old := resolvConfPath
	defer func() { resolvConfPath = old }()
	dir := t.TempDir()
	resolvConfPath = filepath.Join(dir, "resolv.conf")

	ok, hint := unixDirectResolvConf([]string{"1.1.1.1", "8.8.8.8"}, "corp.internal")
	if !ok {
		t.Fatalf("unixDirectResolvConf write failed: %s", hint)
	}
	body, err := os.ReadFile(resolvConfPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.HasPrefix(s, resolvMarker) {
		t.Error("written resolv.conf is missing gravinet's marker")
	}
	if !strings.Contains(s, "search corp.internal") || !strings.Contains(s, "nameserver 1.1.1.1") || !strings.Contains(s, "nameserver 8.8.8.8") {
		t.Errorf("written resolv.conf missing expected lines:\n%s", s)
	}

	// Clearing (both empty) removes a file gravinet owns...
	ok, _ = unixDirectResolvConf(nil, "")
	if !ok {
		t.Fatal("unixDirectResolvConf clear reported failure")
	}
	if _, err := os.Stat(resolvConfPath); !os.IsNotExist(err) {
		t.Error("unixDirectResolvConf did not remove its own marked file on clear")
	}

	// ...but must never remove a file it doesn't recognize as its own — a
	// pre-existing resolv.conf from the distro, or hand-edited by an operator.
	os.WriteFile(resolvConfPath, []byte("nameserver 192.0.2.1\n"), 0o644)
	ok, _ = unixDirectResolvConf(nil, "")
	if !ok {
		t.Fatal("unixDirectResolvConf clear (unowned file) reported failure")
	}
	if _, err := os.Stat(resolvConfPath); err != nil {
		t.Error("unixDirectResolvConf removed a file it never wrote — an unowned resolv.conf must survive a clear")
	}

	// A symlink (e.g. left over from a DNS manager no longer in use) must be
	// replaced with a real file, never written through.
	real := filepath.Join(dir, "real-target")
	os.WriteFile(real, []byte("should not survive\n"), 0o644)
	os.Remove(resolvConfPath)
	os.Symlink(real, resolvConfPath)
	ok, hint = unixDirectResolvConf([]string{"9.9.9.9"}, "")
	if !ok {
		t.Fatalf("unixDirectResolvConf over a symlink failed: %s", hint)
	}
	if fi, err := os.Lstat(resolvConfPath); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("unixDirectResolvConf left the symlink in place instead of replacing it with a real file")
	}
	if realBody, _ := os.ReadFile(real); strings.Contains(string(realBody), "9.9.9.9") {
		t.Error("unixDirectResolvConf wrote through the symlink into its target instead of replacing the link")
	}
}

// TestSetSearchLineOnlyPreservesNameserver is the specific guarantee the whole
// FreeBSD/OpenBSD cooperation design in the package doc depends on: editing
// the search domain must never disturb a "nameserver 127.0.0.1" line pointing
// at unbound, which is what keeps Mesh DNS's own forwarding alive.
func TestSetSearchLineOnlyPreservesNameserver(t *testing.T) {
	old := resolvConfPath
	defer func() { resolvConfPath = old }()
	dir := t.TempDir()
	resolvConfPath = filepath.Join(dir, "resolv.conf")
	os.WriteFile(resolvConfPath, []byte("# hand-tuned\nnameserver 127.0.0.1\noptions edns0\n"), 0o644)

	if err := setSearchLineOnly("corp.internal"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(resolvConfPath)
	s := string(body)
	for _, keep := range []string{"# hand-tuned", "nameserver 127.0.0.1", "options edns0"} {
		if !strings.Contains(s, keep) {
			t.Errorf("setSearchLineOnly dropped %q, must never touch anything but the search line:\n%s", keep, s)
		}
	}
	if !strings.Contains(s, "search corp.internal") {
		t.Errorf("setSearchLineOnly didn't add the search line:\n%s", s)
	}

	// Clearing the search domain removes just that line, still leaving
	// nameserver untouched.
	if err := setSearchLineOnly(""); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(resolvConfPath)
	s = string(body)
	if strings.Contains(s, "search ") {
		t.Errorf("setSearchLineOnly(\"\") should remove the search line:\n%s", s)
	}
	if !strings.Contains(s, "nameserver 127.0.0.1") {
		t.Errorf("setSearchLineOnly(\"\") disturbed the nameserver line:\n%s", s)
	}
}

// TestRootForwardStatePersistence covers the FreeBSD/OpenBSD "." zone
// breadcrumb ReapplyBoot depends on: save, load, and that an empty save
// clears rather than leaving a stale file (which loadRootForwardState would
// otherwise happily reapply on the next boot after the operator meant to
// clear it).
func TestRootForwardStatePersistence(t *testing.T) {
	old := systemResolverStateDir
	defer func() { systemResolverStateDir = old }()
	systemResolverStateDir = t.TempDir()

	if got := loadRootForwardState("freebsd"); got != nil {
		t.Errorf("loadRootForwardState with nothing saved = %v, want nil", got)
	}
	if err := saveRootForwardState("freebsd", []string{"1.1.1.1", "8.8.8.8"}); err != nil {
		t.Fatal(err)
	}
	got := loadRootForwardState("freebsd")
	if strings.Join(got, ",") != "1.1.1.1,8.8.8.8" {
		t.Errorf("loadRootForwardState = %v, want [1.1.1.1 8.8.8.8]", got)
	}
	// Platforms don't share state — an OpenBSD read must not see FreeBSD's file.
	if got := loadRootForwardState("openbsd"); got != nil {
		t.Errorf("loadRootForwardState(openbsd) = %v, want nil (disjoint from freebsd's)", got)
	}
	// Saving an empty list clears the breadcrumb rather than persisting an
	// empty-but-present file.
	if err := saveRootForwardState("freebsd", nil); err != nil {
		t.Fatal(err)
	}
	if got := loadRootForwardState("freebsd"); got != nil {
		t.Errorf("loadRootForwardState after an empty save = %v, want nil", got)
	}
	if _, err := os.Stat(rootForwardStatePath("freebsd")); !os.IsNotExist(err) {
		t.Error("an empty save should remove the state file, not leave an empty one behind")
	}
}

// TestRootForwardStatePathsAreDisjointFromMeshTags guards against a collision
// with internal/resolver's own per-network state files, which key on a
// 16-hex-digit network tag (fmt.Sprintf("%016x", id)) — "__system__" can never
// parse as one, so a listing of the shared directory can never mistake one for
// the other.
func TestRootForwardStatePathsAreDisjointFromMeshTags(t *testing.T) {
	for _, platform := range []string{"freebsd", "openbsd"} {
		name := filepath.Base(rootForwardStatePath(platform))
		if len(strings.TrimSuffix(name, ".json")) == 16 {
			t.Errorf("rootForwardStatePath(%q) = %q looks like it could collide with a 16-hex-digit network tag", platform, name)
		}
		if !strings.Contains(name, "__system__") {
			t.Errorf("rootForwardStatePath(%q) = %q should carry the reserved __system__ marker", platform, name)
		}
	}
}

// TestSetRootForwardSurfacesMissingBinary confirms a bogus/missing control
// binary produces a clean (ok=false, hint) rather than a panic — the same
// "nothing here is exercised against a real daemon on this test host, but the
// failure path must still behave" coverage hosttime_test.go accepts for its
// own resolvectl/w32tm calls.
func TestSetRootForwardSurfacesMissingBinary(t *testing.T) {
	old := systemResolverStateDir
	defer func() { systemResolverStateDir = old }()
	systemResolverStateDir = t.TempDir()

	ok, hint := setRootForward("gravinet-test-nonexistent-control-binary", "freebsd", []string{"1.1.1.1"})
	if ok {
		t.Error("setRootForward with a nonexistent control binary should not report success")
	}
	if hint == "" {
		t.Error("a failure must carry a hint explaining what went wrong")
	}
}

func TestReapplyBootDoesNotPanic(t *testing.T) {
	old := systemResolverStateDir
	defer func() { systemResolverStateDir = old }()
	systemResolverStateDir = t.TempDir()
	ReapplyBoot() // no state saved anywhere; must be a quiet no-op on every platform
}

// TestHostResolverIsSafeToCall mirrors hosttime_test.go's
// TestHostTimeIsSafeToCall: the read path must render on a host with none of
// the tooling it looks for, and never claim a capability is available while
// also explaining why it isn't.
func TestHostResolverIsSafeToCall(t *testing.T) {
	info := HostResolver()
	if info.Hostname == "" {
		t.Error("HostResolver().Hostname is empty; os.Hostname() should always succeed on a real host")
	}
	if !info.CanDNS && info.Hint == "" {
		t.Error("CanDNS is false but Hint is empty; the UI would grey out the DNS card with no explanation")
	}
}

func TestSetHostnameRejectsBadInputBeforeAnyCommand(t *testing.T) {
	for _, name := range []string{"", "host; reboot", "host name", strings.Repeat("a", 300)} {
		if ok, hint := SetHostname(name); ok || hint == "" {
			t.Errorf("SetHostname(%q) = (%v, %q), want a refusal with a reason", name, ok, hint)
		}
	}
}

func TestSetHostDNSValidatesEveryServer(t *testing.T) {
	// The bad entry is second: validation must reject the whole request rather
	// than silently apply the good half.
	ok, hint := SetHostDNS([]string{"1.1.1.1", "evil; reboot"}, "")
	if ok {
		t.Error("SetHostDNS accepted a server list containing a shell metacharacter")
	}
	if !strings.Contains(hint, "evil") {
		t.Errorf("hint %q should name the offending entry", hint)
	}
}

func TestSetHostDNSValidatesSearchDomain(t *testing.T) {
	ok, hint := SetHostDNS([]string{"1.1.1.1"}, "bad domain; reboot")
	if ok {
		t.Error("SetHostDNS accepted a search domain containing whitespace/shell metacharacters")
	}
	if hint == "" {
		t.Error("a rejected search domain must carry a reason")
	}
}

// ── test helpers ────────────────────────────────────────────────────────────

func findLoopback() (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, err
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			return ifi.Index, nil
		}
	}
	return 0, fmt.Errorf("no loopback interface found")
}

func loopbackName(t *testing.T, idx int) string {
	t.Helper()
	ifi, err := net.InterfaceByIndex(idx)
	if err != nil {
		t.Fatalf("could not resolve loopback interface %d by index: %v", idx, err)
	}
	return ifi.Name
}
