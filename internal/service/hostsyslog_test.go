package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidSyslogTargetRejectsInjection: every target that reaches
// SetHostSyslog is written straight into a daemon's own config file (an
// rsyslog action() attribute or a BSD syslog.conf line), so a stray quote,
// newline, or shell metacharacter must be refused before it ever gets
// there — the same reasoning TestValidTimezoneRejectsInjection applies to
// validTimezone.
func TestValidSyslogTargetRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"log.example.com",           // no port
		"log.example.com:",          // empty port
		"log.example.com:0",         // out of range
		"log.example.com:70000",     // out of range
		"log.example.com:notaport",  // non-numeric port
		"evil.com:514\nNTP=evil",    // newline injection
		"evil\".com:514",            // quote injection
		"evil.com; rm -rf /:514",    // shell metacharacters in host
		"$(id).example.com:514",     // command substitution shape
		"`id`.example.com:514",      // backtick shape
		"../../etc/shadow:514",      // path traversal shape
		strings.Repeat("a", 260) + ":514",
	}
	for _, tgt := range bad {
		if err := validSyslogTarget(tgt); err == nil {
			t.Errorf("validSyslogTarget(%q) accepted a value it must reject", tgt)
		}
	}
}

// TestValidSyslogTargetAcceptsRealTargets checks hostnames, IPv4, and
// bracketed IPv6 literals all parse — a remote syslog collector is
// legitimately named any of these ways.
func TestValidSyslogTargetAcceptsRealTargets(t *testing.T) {
	good := []string{
		"log.example.com:514", "10.0.0.1:514", "192.168.1.1:1514",
		"[2001:db8::1]:514", "localhost:601", "syslog-collector.internal:6514",
	}
	for _, tgt := range good {
		if err := validSyslogTarget(tgt); err != nil {
			t.Errorf("validSyslogTarget(%q) rejected a legitimate target: %v", tgt, err)
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
// output to a temp file and checks parseRsyslogDropInAt recovers the same
// target/protocol — the two halves of the "gravinet only ever needs to
// understand its own previously written output" contract the package
// comment describes.
func TestRenderAndParseRsyslogDropInRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gravinet-syslog-forward.conf")
	content := renderRsyslogDropIn("10.0.0.1", "514", "udp")
	if !strings.Contains(content, "Local logging is untouched") {
		t.Error("renderRsyslogDropIn's output doesn't document that local logging is untouched")
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	target, proto, ok := parseRsyslogDropInAt(path)
	if !ok {
		t.Fatal("parseRsyslogDropInAt: expected ok=true reading back a file this package just wrote")
	}
	if target != "10.0.0.1:514" || proto != "udp" {
		t.Errorf("parseRsyslogDropInAt = (%q, %q), want (\"10.0.0.1:514\", \"udp\")", target, proto)
	}
}

func TestParseRsyslogDropInAtMissingFile(t *testing.T) {
	if _, _, ok := parseRsyslogDropInAt(filepath.Join(t.TempDir(), "does-not-exist.conf")); ok {
		t.Error("parseRsyslogDropInAt: expected ok=false for a missing file")
	}
}

// TestRenderAndParseBSDSyslogBlockRoundTrip checks both protocols map to
// the classic BSD syslogd "@"/"@@" forward syntax and back.
func TestRenderAndParseBSDSyslogBlockRoundTrip(t *testing.T) {
	cases := []struct{ target, protocol string }{
		{"10.0.0.1:514", "udp"},
		{"log.example.com:601", "tcp"},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "syslog.conf")
		block := renderBSDSyslogBlock(c.target, c.protocol)
		if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
			t.Fatal(err)
		}
		target, proto, ok := parseBSDSyslogBlock(path)
		if !ok {
			t.Fatalf("parseBSDSyslogBlock: expected ok=true for %+v", c)
		}
		if target != c.target || proto != c.protocol {
			t.Errorf("parseBSDSyslogBlock = (%q, %q), want (%q, %q)", target, proto, c.target, c.protocol)
		}
	}
}

// TestSetSyslogManagedBlockPreservesExistingContent is the test that
// matters most: it checks setSyslogManagedBlock never touches a line it
// didn't write itself — the "still also log locally" requirement this
// whole feature exists to satisfy. Pre-existing default local-logging
// rules must survive being written around, repeatedly, verbatim.
func TestSetSyslogManagedBlockPreservesExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syslog.conf")
	original := "# default local logging, not gravinet's\n" +
		"*.notice;authpriv.none;kern.debug;mail.crit\t/var/log/messages\n" +
		"authpriv.*\t\t\t\t\t/var/log/secure\n" +
		"mail.*\t\t\t\t\t\t-/var/log/maillog\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Enable forwarding: every original line must survive, plus the new
	// managed block appended.
	block := renderBSDSyslogBlock("10.0.0.1:514", "udp")
	if err := setSyslogManagedBlock(path, block); err != nil {
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
	if target, proto, ok := parseBSDSyslogBlock(path); !ok || target != "10.0.0.1:514" || proto != "udp" {
		t.Errorf("managed block not recovered after enabling: (%q, %q, %v)", target, proto, ok)
	}

	// Change the target: original content must still survive, and the
	// block must be replaced (not duplicated).
	block2 := renderBSDSyslogBlock("192.168.1.1:601", "tcp")
	if err := setSyslogManagedBlock(path, block2); err != nil {
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
	if target, proto, ok := parseBSDSyslogBlock(path); !ok || target != "192.168.1.1:601" || proto != "tcp" {
		t.Errorf("managed block not updated: (%q, %q, %v)", target, proto, ok)
	}

	// Disable: original content must still survive, managed block gone.
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
	if _, _, ok := parseBSDSyslogBlock(path); ok {
		t.Error("parseBSDSyslogBlock: expected ok=false after disabling")
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
	if !info.CanSyslog && info.Enabled {
		t.Error("HostSyslog: Enabled true while CanSyslog is false — nothing should be reported configured on an unsupported host")
	}
}

// TestSetHostSyslogRejectsBadInputBeforeTouchingTheSystem checks that a
// validation failure never reaches a real file write or exec.Command —
// safe to run unconditionally on the machine running this test suite,
// regardless of whether rsyslog/syslogd actually happen to be installed
// there (mirrors TestSetHostTimezoneRejectsBadInputBeforeExec).
func TestSetHostSyslogRejectsBadInputBeforeTouchingTheSystem(t *testing.T) {
	if ok, hint := SetHostSyslog(true, "", "udp"); ok || hint == "" {
		t.Errorf(`SetHostSyslog(true, "", "udp") = (%v, %q), want a refusal with a reason`, ok, hint)
	}
	if ok, hint := SetHostSyslog(true, "log.example.com:514", "quic"); ok || hint == "" {
		t.Errorf("SetHostSyslog with a bogus protocol = (%v, %q), want a refusal", ok, hint)
	}
	if ok, hint := SetHostSyslog(true, "not a valid target", "udp"); ok || hint == "" {
		t.Errorf("SetHostSyslog with an unparseable target = (%v, %q), want a refusal", ok, hint)
	}
}
