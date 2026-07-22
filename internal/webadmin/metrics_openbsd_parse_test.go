package webadmin

import "testing"

// TestParseOpenBSDCPUTimeLegacyFiveState covers the pre-6.4 CPUSTATES layout
// (user, nice, sys, intr, idle) — idle must still resolve to the last field.
func TestParseOpenBSDCPUTimeLegacyFiveState(t *testing.T) {
	total, idle, ok := parseOpenBSDCPUTime([]string{"100", "10", "50", "5", "1000"})
	if !ok {
		t.Fatal("expected ok=true for a well-formed 5-field cp_time")
	}
	if want := uint64(100 + 10 + 50 + 5 + 1000); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if idle != 1000 {
		t.Errorf("idle = %d, want 1000 (the last field)", idle)
	}
}

// TestParseOpenBSDCPUTimeSixState covers the OpenBSD 6.4+ layout (user, nice,
// sys, spin, intr, idle) using a real observed `sysctl kern.cp_time` sample
// (interrupt=2391 nice=0 user=1987 system=60 spin=117 idle=976656, per a
// node_exporter bug report against a live OpenBSD 7.3 box) — regardless of
// which field is which, the parser must sum all six for total and take the
// last as idle without needing to know the field layout at all.
func TestParseOpenBSDCPUTimeSixState(t *testing.T) {
	fields := []string{"2391", "0", "1987", "60", "117", "976656"}
	total, idle, ok := parseOpenBSDCPUTime(fields)
	if !ok {
		t.Fatal("expected ok=true for a well-formed 6-field cp_time")
	}
	if want := uint64(2391 + 0 + 1987 + 60 + 117 + 976656); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if idle != 976656 {
		t.Errorf("idle = %d, want 976656 (the last field)", idle)
	}
}

// TestParseOpenBSDCPUTimeRejectsShortOrBadInput ensures a too-short or
// non-numeric field list fails closed (ok=false) instead of returning a
// bogus total/idle.
func TestParseOpenBSDCPUTimeRejectsShortOrBadInput(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"1", "2", "3"}, // fewer than 5 fields
		{"1", "2", "3", "4", "not-a-number"},
	}
	for _, fields := range cases {
		if _, _, ok := parseOpenBSDCPUTime(fields); ok {
			t.Errorf("parseOpenBSDCPUTime(%v) should fail, got ok=true", fields)
		}
	}
}

// TestParseTopSize covers top(1)'s bare-KB and K/M/G/T-suffixed size tokens.
func TestParseTopSize(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"512", 512},          // bare number: already KB
		{"512K", 512},
		{"733M", 733 * 1024},
		{"11G", 11 * 1024 * 1024},
		{"2T", 2 * 1024 * 1024 * 1024},
		{"5m", 5 * 1024}, // lowercase suffix
	}
	for _, tc := range cases {
		got, ok := parseTopSize(tc.in)
		if !ok || got != tc.want {
			t.Errorf("parseTopSize(%q) = (%d, %v), want (%d, true)", tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "M", "abcK"} {
		if _, ok := parseTopSize(bad); ok {
			t.Errorf("parseTopSize(%q) should fail, got ok=true", bad)
		}
	}
}

// TestParseOpenBSDMemLine checks the full "Memory:" line parse against real
// observed top(1) output (from OpenBSD sysadmin write-ups), and locks in the
// tot+free=total, tot-cache=used formula documented on parseOpenBSDMemLine.
func TestParseOpenBSDMemLine(t *testing.T) {
	// "Real: 244M/733M act/tot Free: 234M Cache: 193M Swap: 158M/752M"
	// tot=733M free=234M cache=193M -> total=967M used=733M-193M=540M
	// -> 540/967*100 = 55.8428...%
	line := []byte("Memory: Real: 244M/733M act/tot Free: 234M Cache: 193M Swap: 158M/752M")
	pct, ok := parseOpenBSDMemLine(line)
	if !ok {
		t.Fatal("expected ok=true for a well-formed Memory line")
	}
	want := float64(733-193) / float64(733+234) * 100
	if diff := pct - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("pct = %v, want %v", pct, want)
	}

	// A line embedded in a larger top(1) dump (with a process table below
	// it) must still parse — the regexp shouldn't require the line to be the
	// entire input.
	full := []byte("load averages: 0.10, 0.05, 0.01\nhost 12:00:00\n" +
		"1 processes: 1 idle\n" +
		"CPU states: 0.0% user, 0.0% nice, 0.0% system, 0.0% interrupt, 100% idle\n" +
		"Memory: Real: 701M/1254M act/tot Free: 681M Cache: 387M Swap: 72M/11G\n" +
		"  PID USERNAME PRI NICE  SIZE   RES STATE     WAIT      TIME    CPU COMMAND\n")
	if _, ok := parseOpenBSDMemLine(full); !ok {
		t.Fatal("expected ok=true when the Memory line is embedded in a full top(1) dump")
	}

	if _, ok := parseOpenBSDMemLine([]byte("nothing useful here")); ok {
		t.Fatal("expected ok=false when no Memory line is present")
	}
}

// TestParseOpenBSDMemLineExcludesCache is a regression test for the specific
// design choice (mirroring metrics_freebsd.go): reclaimable buffer-cache
// pages must not count as "used". A line where cache is a large fraction of
// tot should report noticeably less than 100% used, not close to it.
func TestParseOpenBSDMemLineExcludesCache(t *testing.T) {
	// tot=1000M, cache=900M -> used=100M against total=1000M+0M(free)=1000M -> 10%
	line := []byte("Memory: Real: 50M/1000M act/tot Free: 0M Cache: 900M Swap: 0M/0M")
	pct, ok := parseOpenBSDMemLine(line)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pct > 15 {
		t.Errorf("pct = %v, want roughly 10%% once cache is excluded from used", pct)
	}
}

// TestSplitOpenBSDSysctlList is a regression test for the bug that broke the
// CPU metric outright: OpenBSD's sysctl joins array values with commas, not
// spaces, so strings.Fields alone left the whole value as one unparseable
// token.
func TestSplitOpenBSDSysctlList(t *testing.T) {
	// Real observed `sysctl kern.cp_time` value (minus the "kern.cp_time="
	// prefix, which -n suppresses) from a live OpenBSD 7.3 box.
	got := splitOpenBSDSysctlList("2391,0,1987,60,117,976656\n")
	want := []string{"2391", "0", "1987", "60", "117", "976656"}
	if len(got) != len(want) {
		t.Fatalf("splitOpenBSDSysctlList(...) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReadCPUTotalsPipelineWithCommaOutput exercises the exact pipeline
// readCPUTotals uses (split then parse) against real comma-separated sysctl
// output end to end, since parseOpenBSDCPUTime alone (fed pre-split []string
// in the other tests above) can't catch a bug in how the raw string gets
// split in the first place — which is exactly what broke here.
func TestReadCPUTotalsPipelineWithCommaOutput(t *testing.T) {
	total, idle, ok := parseOpenBSDCPUTime(splitOpenBSDSysctlList("2391,0,1987,60,117,976656\n"))
	if !ok {
		t.Fatal("expected ok=true for real comma-separated sysctl output")
	}
	if want := uint64(2391 + 0 + 1987 + 60 + 117 + 976656); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if idle != 976656 {
		t.Errorf("idle = %d, want 976656", idle)
	}
}

// TestParseOpenBSDNetstatLinkRow covers the ordinary NIC case: a "<Link>"
// row plus per-address-family duplicate rows, where the Link row's totals
// must win.
func TestParseOpenBSDNetstatLinkRow(t *testing.T) {
	raw := "Name    Mtu   Network     Address               Ibytes     Obytes\n" +
		"em0     1500  <Link>      00:11:22:33:44:55     1000       2000\n" +
		"em0     1500  10.0.0/24   10.0.0.5              900        1800\n"
	dev := parseOpenBSDNetstat([]byte(raw))
	c, ok := dev["em0"]
	if !ok {
		t.Fatal("expected em0 to be present")
	}
	if c.rx != 1000 || c.tx != 2000 {
		t.Errorf("em0 = %+v, want rx=1000 tx=2000 (the Link row, not the address row)", c)
	}
}

// TestParseOpenBSDNetstatNoLinkRow is a regression test for the hypothesis
// that broke network metrics: a point-to-point interface (like gravinet's
// own tun devices) might have no "<Link>" row at all, only a per-address
// row. Before this fix, requiring a "<Link" prefix meant such an interface
// never appeared in the result at all.
func TestParseOpenBSDNetstatNoLinkRow(t *testing.T) {
	raw := "Name    Mtu   Network     Address               Ibytes     Obytes\n" +
		"tun0    1500  10.1.2/24   10.1.2.1              500        700\n"
	dev := parseOpenBSDNetstat([]byte(raw))
	c, ok := dev["tun0"]
	if !ok {
		t.Fatal("expected tun0 to be present even with no Link row")
	}
	if c.rx != 500 || c.tx != 700 {
		t.Errorf("tun0 = %+v, want rx=500 tx=700", c)
	}
}

// TestParseOpenBSDNetstatDownInterface is a regression test for the other
// hypothesis: netstat marks a down interface with a trailing "*" on its
// name (see netstat(1)), which must be stripped so the map key matches the
// plain interface name callers look up by.
func TestParseOpenBSDNetstatDownInterface(t *testing.T) {
	raw := "Name    Mtu   Network     Address               Ibytes     Obytes\n" +
		"tun1*   1500  <Link>      tun1                   10         20\n"
	dev := parseOpenBSDNetstat([]byte(raw))
	if _, ok := dev["tun1*"]; ok {
		t.Fatal("the map key must not include the trailing down-interface '*'")
	}
	c, ok := dev["tun1"]
	if !ok {
		t.Fatal("expected tun1 (asterisk stripped) to be present")
	}
	if c.rx != 10 || c.tx != 20 {
		t.Errorf("tun1 = %+v, want rx=10 tx=20", c)
	}
}

// TestParseOpenBSDNetstatMalformedRow ensures a short/malformed row is
// skipped rather than misparsed or causing a panic.
func TestParseOpenBSDNetstatMalformedRow(t *testing.T) {
	raw := "Name    Mtu   Network     Address               Ibytes     Obytes\n" +
		"weird   1500\n" +
		"em0     1500  <Link>      00:11:22:33:44:55     42         84\n"
	dev := parseOpenBSDNetstat([]byte(raw))
	if _, ok := dev["weird"]; ok {
		t.Fatal("a too-short row must not produce an entry")
	}
	if c, ok := dev["em0"]; !ok || c.rx != 42 || c.tx != 84 {
		t.Errorf("em0 = %+v ok=%v, want rx=42 tx=84 ok=true", c, ok)
	}
}
