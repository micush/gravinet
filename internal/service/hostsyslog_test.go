package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidSyslogHostRejectsInjection: every host that reaches
// SetHostSyslog is written straight into a daemon's own config file (an
// rsyslog action() attribute or a BSD syslog.conf line), so a stray quote,
// newline, or shell metacharacter must be refused before it ever gets
// there — the same reasoning TestValidTimezoneRejectsInjection applies to
// validTimezone.
func TestValidSyslogHostRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"evil.com\nNTP=evil",     // newline injection
		`evil".com`,              // quote injection
		"evil.com; rm -rf /",     // shell metacharacters
		"$(id).example.com",      // command substitution shape
		"`id`.example.com",       // backtick shape
		"../../etc/shadow",       // path traversal shape
		strings.Repeat("a", 260), // too long
	}
	for _, h := range bad {
		if err := validSyslogHost(h); err == nil {
			t.Errorf("validSyslogHost(%q) accepted a value it must reject", h)
		}
	}
}

// TestValidSyslogHostAcceptsRealHosts checks hostnames and both IPv4 and
// IPv6 literals all parse — a remote syslog collector is legitimately
// named any of these ways. Unlike the old combined host:port form, an
// IPv6 literal here needs no brackets: Remote and Port are separate
// structured fields.
func TestValidSyslogHostAcceptsRealHosts(t *testing.T) {
	good := []string{
		"log.example.com", "10.0.0.1", "192.168.1.1",
		"2001:db8::1", "localhost", "syslog-collector.internal",
	}
	for _, h := range good {
		if err := validSyslogHost(h); err != nil {
			t.Errorf("validSyslogHost(%q) rejected a legitimate host: %v", h, err)
		}
	}
}

// TestAttrValue exercises the small extractor parseRsyslogDropInAt relies
// on, directly, against both a realistic action() line and edge cases
// (missing key, unterminated quote).
func TestAttrValue(t *testing.T) {
	line := `*.* action(type="omfwd" target="10.0.0.1" port="514" protocol="udp")`
	cases := []struct{ key, want string }{
		{"target", "10.0.0.1"},
		{"port", "514"},
		{"protocol", "udp"},
		{"type", "omfwd"},
		{"missing", ""},
	}
	for _, c := range cases {
		if got := attrValue(line, c.key); got != c.want {
			t.Errorf("attrValue(line, %q) = %q, want %q", c.key, got, c.want)
		}
	}
	if got := attrValue(`target=`, "target"); got != "" {
		t.Errorf("attrValue with no opening quote = %q, want empty", got)
	}
	if got := attrValue(`target="unterminated`, "target"); got != "" {
		t.Errorf("attrValue with no closing quote = %q, want empty", got)
	}
}

// TestRenderAndParseRsyslogDropInRoundTrip writes renderRsyslogDropIn's
// output (multiple targets, mixed enabled/disabled) to a temp file and
// checks parseRsyslogDropInAt recovers exactly the same set, in order —
// the two halves of the "gravinet only ever needs to understand its own
// previously written output" contract the package comment describes.
func TestRenderAndParseRsyslogDropInRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravinet-syslog-forward.conf")
	want := []SyslogTarget{
		{Remote: "10.0.0.1", Port: 514, Protocol: "udp"},
		{Remote: "log.example.com", Port: 6514, Protocol: "tcp", Disabled: true},
	}
	content := renderRsyslogDropIn(want)
	if !strings.Contains(content, "Local logging is untouched") {
		t.Error("renderRsyslogDropIn's output doesn't document that local logging is untouched")
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := parseRsyslogDropInAt(path)
	if !ok {
		t.Fatal("parseRsyslogDropInAt: expected ok=true reading back a file this package just wrote")
	}
	if len(got) != len(want) {
		t.Fatalf("parseRsyslogDropInAt: got %d targets, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseRsyslogDropInAtMissingFile(t *testing.T) {
	if _, ok := parseRsyslogDropInAt(filepath.Join(t.TempDir(), "does-not-exist.conf")); ok {
		t.Error("parseRsyslogDropInAt: expected ok=false for a missing file")
	}
}

// TestRenderAndParseBSDSyslogBlockRoundTrip checks both protocols map to
// the classic BSD syslogd "@"/"@@" forward syntax and back, across
// multiple targets including a disabled one.
func TestRenderAndParseBSDSyslogBlockRoundTrip(t *testing.T) {
	want := []SyslogTarget{
		{Remote: "10.0.0.1", Port: 514, Protocol: "udp"},
		{Remote: "log.example.com", Port: 601, Protocol: "tcp"},
		{Remote: "192.168.1.1", Port: 514, Protocol: "udp", Disabled: true},
	}
	path := filepath.Join(t.TempDir(), "syslog.conf")
	block := renderBSDSyslogBlock(want)
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := parseBSDSyslogBlock(path)
	if !ok {
		t.Fatalf("parseBSDSyslogBlock: expected ok=true for %+v", want)
	}
	if len(got) != len(want) {
		t.Fatalf("parseBSDSyslogBlock: got %d targets, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSetSyslogManagedBlockPreservesExistingContent is the test that
// matters most: it checks setSyslogManagedBlock never touches a line it
// didn't write itself — the "still also log locally" requirement this
// whole feature exists to satisfy. Pre-existing default local-logging
// rules must survive being written around, repeatedly, verbatim, and the
// managed block itself must carry more than one target across a save.
func TestSetSyslogManagedBlockPreservesExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syslog.conf")
	original := "# default local logging, not gravinet's\n" +
		"*.notice;authpriv.none;kern.debug;mail.crit\t/var/log/messages\n" +
		"authpriv.*\t\t\t\t\t/var/log/secure\n" +
		"mail.*\t\t\t\t\t\t-/var/log/maillog\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Enable forwarding with two targets: every original line must
	// survive, plus the new managed block appended.
	targets := []SyslogTarget{
		{Remote: "10.0.0.1", Port: 514, Protocol: "udp"},
		{Remote: "192.168.1.1", Port: 601, Protocol: "tcp"},
	}
	if err := setSyslogManagedBlock(path, renderBSDSyslogBlock(targets)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(original, "\n"), "\n") {
		if !strings.Contains(string(got), line) {
			t.Errorf("original local-logging line dropped: %q\nfull file:\n%s", line, got)
		}
	}
	parsed, ok := parseBSDSyslogBlock(path)
	if !ok || len(parsed) != 2 {
		t.Fatalf("managed block not recovered after enabling: ok=%v parsed=%+v", ok, parsed)
	}

	// Replace with a single target: original content must still survive,
	// and the block must be replaced (not duplicated), not merged.
	targets2 := []SyslogTarget{{Remote: "192.168.1.1", Port: 601, Protocol: "tcp"}}
	if err := setSyslogManagedBlock(path, renderBSDSyslogBlock(targets2)); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimRight(original, "\n"), "\n") {
		if !strings.Contains(string(got2), line) {
			t.Errorf("original local-logging line dropped after a second write: %q", line)
		}
	}
	if strings.Count(string(got2), "BEGIN gravinet syslog-forward") != 1 {
		t.Errorf("expected exactly one managed block after replacing it, file:\n%s", got2)
	}
	parsed2, ok := parseBSDSyslogBlock(path)
	if !ok || len(parsed2) != 1 || parsed2[0] != targets2[0] {
		t.Errorf("managed block not updated: ok=%v parsed=%+v", ok, parsed2)
	}

	// Disable entirely: original content must still survive, managed
	// block gone.
	if err := setSyslogManagedBlock(path, ""); err != nil {
		t.Fatal(err)
	}
	got3, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimRight(original, "\n"), "\n") {
		if !strings.Contains(string(got3), line) {
			t.Errorf("original local-logging line dropped after disabling: %q", line)
		}
	}
	if strings.Contains(string(got3), "BEGIN gravinet syslog-forward") {
		t.Errorf("managed block still present after disabling, file:\n%s", got3)
	}
	if _, ok := parseBSDSyslogBlock(path); ok {
		t.Error("parseBSDSyslogBlock: expected ok=false after disabling")
	}
}

// TestBSDSyslogBlockDisabledTargetRoundTripsAndStaysInert checks the
// specific behavior a per-row "state" toggle depends on: a disabled target
// survives being written and re-read (so unchecking it in the web admin
// and coming back later still shows the row, unchecked) while its line is
// a comment as far as syslogd would parse the file (so it never actually
// forwards).
func TestBSDSyslogBlockDisabledTargetRoundTripsAndStaysInert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syslog.conf")
	targets := []SyslogTarget{{Remote: "10.0.0.1", Port: 514, Protocol: "udp", Disabled: true}}
	if err := os.WriteFile(path, []byte(renderBSDSyslogBlock(targets)), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), rsyslogDisabledPrefix) {
		t.Fatalf("disabled target wasn't written with the disabled marker:\n%s", raw)
	}
	got, ok := parseBSDSyslogBlock(path)
	if !ok || len(got) != 1 || !got[0].Disabled {
		t.Errorf("disabled target didn't round-trip: ok=%v got=%+v", ok, got)
	}
}

// TestSyslogSupportedAndHostSyslogAreSafeToCall mirrors
// TestHostTimeIsSafeToCall: these must never panic or hang regardless of
// what's installed on the machine running the test suite, and CanSyslog
// must always come with a Hint when false.
func TestSyslogSupportedAndHostSyslogAreSafeToCall(t *testing.T) {
	ok, hint := SyslogSupported()
	if !ok && hint == "" {
		t.Error("SyslogSupported: false with no hint; the UI would show an empty reason")
	}
	info := HostSyslog()
	if info.CanSyslog != ok {
		t.Errorf("HostSyslog().CanSyslog = %v, want it to match SyslogSupported() = %v", info.CanSyslog, ok)
	}
	if !info.CanSyslog && len(info.Targets) != 0 {
		t.Error("HostSyslog: targets reported while CanSyslog is false — nothing should be reported configured on an unsupported host")
	}
}

// TestSetHostSyslogRejectsBadInputBeforeTouchingTheSystem checks that a
// validation failure never reaches a real file write or exec.Command —
// safe to run unconditionally on the machine running this test suite,
// regardless of whether rsyslog/syslogd actually happen to be installed
// there (mirrors TestSetHostTimezoneRejectsBadInputBeforeExec). Also
// checks that one bad entry among several rejects the whole batch rather
// than partially applying it.
func TestSetHostSyslogRejectsBadInputBeforeTouchingTheSystem(t *testing.T) {
	cases := []struct {
		name    string
		targets []SyslogTarget
	}{
		{"empty remote", []SyslogTarget{{Remote: "", Port: 514, Protocol: "udp"}}},
		{"bad protocol", []SyslogTarget{{Remote: "log.example.com", Port: 514, Protocol: "quic"}}},
		{"unparseable host", []SyslogTarget{{Remote: "not a valid host", Port: 514, Protocol: "udp"}}},
		{"port out of range", []SyslogTarget{{Remote: "log.example.com", Port: 0, Protocol: "udp"}}},
		{"one bad entry among good ones", []SyslogTarget{
			{Remote: "log.example.com", Port: 514, Protocol: "udp"},
			{Remote: "log2.example.com", Port: 99999, Protocol: "udp"},
		}},
	}
	for _, c := range cases {
		if ok, hint := SetHostSyslog(c.targets); ok || hint == "" {
			t.Errorf("%s: SetHostSyslog(%+v) = (%v, %q), want a refusal with a reason", c.name, c.targets, ok, hint)
		}
	}
}
