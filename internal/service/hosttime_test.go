package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidTimezoneRejectsInjection is the important half of this file: every
// timezone that reaches SetHostTimezone becomes both an exec argument and a
// path join under /usr/share/zoneinfo, and the value arrives from a browser
// field. Shell metacharacters, traversal, and absolute paths must all be
// refused before either use.
func TestValidTimezoneRejectsInjection(t *testing.T) {
	bad := []string{
		"America/Phoenix; rm -rf /",
		"America/Phoenix && curl example.com",
		"America/Phoenix|sh",
		"$(id)",
		"`id`",
		"../../etc/shadow",
		"Europe/..%2fPassword",
		"/etc/localtime",
		"UTC\nNTP=evil",
		strings.Repeat("A", 200),
	}
	for _, tz := range bad {
		if err := validTimezone(tz); err == nil {
			t.Errorf("validTimezone(%q) accepted a value it must reject", tz)
		}
	}
}

func TestValidTimezoneAcceptsRealZones(t *testing.T) {
	// IANA names, plus the Windows zone ids tzutil actually uses — those carry
	// spaces, which is why spaces are allowed at all.
	good := []string{
		"UTC", "Etc/UTC", "America/Phoenix", "America/Argentina/Buenos_Aires",
		"Europe/London", "Asia/Kolkata", "Etc/GMT+7",
		"US Mountain Standard Time", "W. Europe Standard Time",
	}
	for _, tz := range good {
		if err := validTimezone(tz); err != nil {
			t.Errorf("validTimezone(%q) rejected a legitimate zone name: %v", tz, err)
		}
	}
	if err := validTimezone(""); err != nil {
		t.Errorf("validTimezone(%q) should be the caller's problem, not a charset error: %v", "", err)
	}
}

func TestValidNTPServer(t *testing.T) {
	for _, s := range []string{"0.pool.ntp.org", "time.apple.com", "10.0.0.1", "2001:db8::1", "[2001:db8::1]", "fe80::1%eth0", "ntp-1_internal"} {
		if err := validNTPServer(s); err != nil {
			t.Errorf("validNTPServer(%q) rejected a legitimate address: %v", s, err)
		}
	}
	for _, s := range []string{"pool.ntp.org && curl evil", "a b", "$(id)", "x;y", "host|sh", "a\nb", strings.Repeat("a", 300)} {
		if err := validNTPServer(s); err == nil {
			t.Errorf("validNTPServer(%q) accepted a value it must reject", s)
		}
	}
}

func TestParseLocalDateTime(t *testing.T) {
	// datetime-local sends seconds only sometimes (it depends on the step
	// attribute and the browser), so both shapes have to parse.
	for _, s := range []string{"2026-07-23T14:30", "2026-07-23T14:30:05", "2026-07-23 14:30", "2026-07-23 14:30:05"} {
		got, err := parseLocalDateTime(s)
		if err != nil {
			t.Fatalf("parseLocalDateTime(%q): %v", s, err)
		}
		if got.Year() != 2026 || got.Month() != time.July || got.Day() != 23 || got.Hour() != 14 || got.Minute() != 30 {
			t.Errorf("parseLocalDateTime(%q) = %v, want 2026-07-23 14:30 local", s, got)
		}
		if got.Location() != time.Local {
			t.Errorf("parseLocalDateTime(%q) parsed in %v, want the host's local zone — a UTC reading would set the clock off by the offset", s, got.Location())
		}
	}
	for _, s := range []string{"", "not-a-date", "23/07/2026 14:30", "2026-07-23T14", "14:30"} {
		if _, err := parseLocalDateTime(s); err == nil {
			t.Errorf("parseLocalDateTime(%q) accepted a malformed value", s)
		}
	}
}

// TestIsDirectivePrefixBoundary guards the bug this helper exists to avoid:
// naive prefix matching would treat FallbackNTP= as an NTP= line and delete a
// setting the operator meant to keep.
func TestIsDirectivePrefixBoundary(t *testing.T) {
	cases := []struct {
		line, keyword string
		want          bool
	}{
		{"NTP=a b", "NTP", true},
		{"NTP a b", "NTP", true},
		{"FallbackNTP=a", "NTP", false},
		{"NTPFoo=a", "NTP", false},
		{"server 1.2.3.4", "server", true},
		{"servertimeout 5", "server", false},
		{"servers pool.example", "server", false},
		{"SERVER 1.2.3.4", "server", true}, // config keywords are case-insensitive
		{"NTP", "NTP", false},              // bare keyword sets nothing
		{"", "NTP", false},
	}
	for _, c := range cases {
		if got := isDirective(c.line, c.keyword); got != c.want {
			t.Errorf("isDirective(%q, %q) = %v, want %v", c.line, c.keyword, got, c.want)
		}
	}
}

func TestDirectiveValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chrony.conf")
	body := strings.Join([]string{
		"# a distro's commented-out example must not show up as configured",
		"#server 0.example.pool.ntp.org iburst",
		"; nor this one",
		";pool 1.example.pool.ntp.org",
		"driftfile /var/lib/chrony/drift",
		"server 10.0.0.1 iburst", // options after the address are not addresses
		"pool 2.pool.ntp.org maxsources 4",
		"makestep 1.0 3",
		"servertimeout 5", // must not match "server"
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := directiveValues(p, "server", "pool")
	want := []string{"10.0.0.1", "2.pool.ntp.org"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directiveValues = %v, want %v", got, want)
	}

	// An NTP= line is a whole space-separated list, so every field counts.
	p2 := filepath.Join(dir, "timesyncd.conf")
	if err := os.WriteFile(p2, []byte("[Time]\nNTP=a.example b.example\nFallbackNTP=c.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = directiveValues(p2, "NTP")
	if strings.Join(got, ",") != "a.example,b.example" {
		t.Errorf("directiveValues(NTP) = %v, want [a.example b.example] — FallbackNTP must not be folded in", got)
	}

	if v := directiveValues(filepath.Join(dir, "nope.conf"), "server"); v != nil {
		t.Errorf("directiveValues on a missing file = %v, want nil", v)
	}
}

// TestSetDirectiveLinesPreservesFile is the behaviour that motivated writing
// this rather than reusing parapet's whole-file rewrite: everything the operator
// or the distro put in the file has to survive a server-list change.
func TestSetDirectiveLinesPreservesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ntp.conf")
	body := strings.Join([]string{
		"# hand-tuned, do not lose me",
		"driftfile /var/db/ntpd.drift",
		"server old-one.example iburst",
		"restrict default limited kod nomodify notrap noquery nopeer",
		"server old-two.example",
		"leapfile /var/db/ntpd.leap-seconds.list",
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setDirectiveLines(p, []string{"server", "pool"}, prefixEach("server ", []string{"new-one.example", "new-two.example"})); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, keep := range []string{"# hand-tuned, do not lose me", "driftfile /var/db/ntpd.drift", "restrict default limited", "leapfile /var/db/ntpd.leap-seconds.list"} {
		if !strings.Contains(got, keep) {
			t.Errorf("setDirectiveLines dropped %q:\n%s", keep, got)
		}
	}
	for _, gone := range []string{"old-one.example", "old-two.example"} {
		if strings.Contains(got, gone) {
			t.Errorf("setDirectiveLines left the old server %q behind:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "server new-one.example") || !strings.Contains(got, "server new-two.example") {
		t.Errorf("setDirectiveLines didn't write the new servers:\n%s", got)
	}
	// The replacements land where the first dropped line was, so a file whose
	// ordering someone chose keeps its shape.
	lines := strings.Split(got, "\n")
	if lines[2] != "server new-one.example" || lines[3] != "server new-two.example" {
		t.Errorf("replacement lines should sit where the first server line was, got:\n%s", got)
	}
	// Permissions carry over — a 0600 ntp.conf must not silently widen to 0644.
	if fi, err := os.Stat(p); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 preserved", fi.Mode().Perm())
	}
}

func TestSetDirectiveLinesAppendsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ntpd.conf")
	if err := os.WriteFile(p, []byte("# no servers here yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setDirectiveLines(p, []string{"server", "servers"}, []string{"server a.example"}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "# no servers here yet") || !strings.Contains(string(out), "server a.example") {
		t.Errorf("append case lost content or didn't append:\n%s", out)
	}
}

// TestSetTimesyncdServers covers the three shapes of timesyncd.conf a host can
// be in: one with an NTP= line to replace, one with a [Time] section but no
// NTP=, and no file at all.
func TestSetTimesyncdServers(t *testing.T) {
	dir := t.TempDir()

	t.Run("replaces NTP and keeps everything else", func(t *testing.T) {
		p := filepath.Join(dir, "a.conf")
		body := "# distro comment\n[Time]\nNTP=stale.example\nFallbackNTP=fallback.example\nPollIntervalMaxSec=2048\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := setTimesyncdServersAt(p, []string{"a.example", "b.example"}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		s := string(got)
		if strings.Contains(s, "stale.example") {
			t.Errorf("old NTP= survived:\n%s", s)
		}
		if !strings.Contains(s, "NTP=a.example b.example") {
			t.Errorf("new NTP= missing:\n%s", s)
		}
		// The whole reason this isn't a wholesale rewrite:
		for _, keep := range []string{"# distro comment", "FallbackNTP=fallback.example", "PollIntervalMaxSec=2048"} {
			if !strings.Contains(s, keep) {
				t.Errorf("lost %q — a server-list save must not discard other settings:\n%s", keep, s)
			}
		}
		if strings.Count(s, "NTP=a.example") != 1 {
			t.Errorf("NTP= written more than once:\n%s", s)
		}
	})

	t.Run("inserts under an existing [Time]", func(t *testing.T) {
		p := filepath.Join(dir, "b.conf")
		if err := os.WriteFile(p, []byte("[Time]\nFallbackNTP=f.example\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := setTimesyncdServersAt(p, []string{"a.example"}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		lines := strings.Split(string(got), "\n")
		if lines[0] != "[Time]" || lines[1] != "NTP=a.example" {
			t.Errorf("NTP= must land directly under [Time], got:\n%s", got)
		}
	})

	t.Run("creates the file when missing", func(t *testing.T) {
		p := filepath.Join(dir, "missing.conf")
		if err := setTimesyncdServersAt(p, []string{"a.example"}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "[Time]") || !strings.Contains(string(got), "NTP=a.example") {
			t.Errorf("created file is not a usable timesyncd.conf:\n%s", got)
		}
	})
}

func TestWindowsPeerList(t *testing.T) {
	cases := []struct{ in, want string }{
		{"time.windows.com,0x9 (Local)", "time.windows.com"},
		{"a.example,0x9 b.example,0x9", "a.example,b.example"},
		{"time.windows.com", "time.windows.com"},
		{"", ""},
	}
	for _, c := range cases {
		got := strings.Join(windowsPeerList(c.in), ",")
		if got != c.want {
			t.Errorf("windowsPeerList(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKeyValsAndLineHelpers(t *testing.T) {
	kv := keyVals("Timezone=America/Phoenix\nNTP=yes\nNTPSynchronized=no\ngarbage\n")
	if kv["Timezone"] != "America/Phoenix" || kv["NTP"] != "yes" || kv["NTPSynchronized"] != "no" {
		t.Errorf("keyVals parsed timedatectl output wrong: %v", kv)
	}
	if got := afterColon("Time Zone: America/Phoenix"); got != "America/Phoenix" {
		t.Errorf("afterColon = %q", got)
	}
	if got := afterColon("Network Time: On"); got != "On" {
		t.Errorf("afterColon = %q", got)
	}
	if got := afterColon("no colon here"); got != "" {
		t.Errorf("afterColon on a colonless line = %q, want empty", got)
	}
	if got := afterColon(lineContaining("Leap Indicator: 0(no warning)\nSource: time.example\n", "Source:")); got != "time.example" {
		t.Errorf("lineContaining+afterColon = %q", got)
	}
	if got := lineContaining("a\nb\n", "zzz"); got != "" {
		t.Errorf("lineContaining with no match = %q, want empty", got)
	}
}

// TestHostTimeIsSafeToCall checks the read path can't blow up or lie on a host
// with none of the tooling it looks for — the page has to render regardless, and
// "couldn't tell" has to stay distinguishable from "off".
func TestHostTimeIsSafeToCall(t *testing.T) {
	info := HostTime()
	if info.Now.IsZero() {
		t.Error("HostTime returned a zero clock; Now comes from this process and is always available")
	}
	if d := time.Since(info.Now); d > time.Minute || d < -time.Minute {
		t.Errorf("HostTime().Now is %v away from time.Now()", d)
	}
	if info.TimezoneStyle != "iana" && info.TimezoneStyle != "windows" {
		t.Errorf("TimezoneStyle = %q, want iana or windows", info.TimezoneStyle)
	}
	if !info.NTPKnown && info.NTPEnabled {
		t.Error("NTPEnabled set while NTPKnown is false — that combination claims knowledge the host never gave us")
	}
	if !info.SyncKnown && info.Synchronized {
		t.Error("Synchronized set while SyncKnown is false")
	}
	// Whenever something is unavailable, say why: a greyed-out control with no
	// explanation is the failure mode the Hint field exists to prevent.
	if (!info.CanNTP || !info.CanTimezone || !info.CanClock) && info.Hint == "" {
		t.Error("a Can* flag is false but Hint is empty; the UI would grey a control out with no reason shown")
	}
}

func TestSetHostClockRejectsBadInput(t *testing.T) {
	// No command runs for these: they're refused during parsing, so this stays
	// safe on a machine running the test suite.
	if ok, hint := SetHostClock(""); ok || hint == "" {
		t.Errorf("SetHostClock(\"\") = (%v, %q), want a refusal with a reason", ok, hint)
	}
	if ok, _ := SetHostClock("tomorrow-ish"); ok {
		t.Error("SetHostClock accepted an unparseable datetime")
	}
}

func TestSetHostTimezoneRejectsBadInputBeforeExec(t *testing.T) {
	for _, tz := range []string{"", "../../etc", "UTC; reboot"} {
		if ok, hint := SetHostTimezone(tz); ok || hint == "" {
			t.Errorf("SetHostTimezone(%q) = (%v, %q), want a refusal with a reason", tz, ok, hint)
		}
	}
}

func TestSetHostNTPValidatesEveryServer(t *testing.T) {
	// The bad entry is second: validation has to reject the whole request rather
	// than write the good half and then fail.
	ok, hint := SetHostNTP(true, []string{"0.pool.ntp.org", "evil; reboot"})
	if ok {
		t.Error("SetHostNTP accepted a server list containing a shell metacharacter")
	}
	if !strings.Contains(hint, "evil") {
		t.Errorf("hint %q should name the offending entry", hint)
	}
}
