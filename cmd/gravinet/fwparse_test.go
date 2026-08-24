package main

import (
	"math"
	"strings"
	"testing"
)

// TestParseIndexRejectsWhatWouldTruncate is the regression test for the
// CodeQL finding. The old code was `int(parseUint(pos[1]))`, which on a
// 64-bit build turns every one of these into a negative number that the
// caller's `dest < 0` check happens to catch — and on linux/arm, which is in
// the release matrix, truncates to the low 32 bits instead. 4294967296
// becomes 0, which for `fw move` means the top of a first-match rulebase.
//
// The values below are chosen so that the 32-bit result is a *plausible*
// position rather than an obviously broken one: that is what makes the bug
// silent.
func TestParseIndexRejectsWhatWouldTruncate(t *testing.T) {
	for _, in := range []string{
		"2147483648",           // MaxInt32 + 1: first value that cannot be an int on arm
		"4294967296",           // 2^32: truncates to 0 — the top of the rulebase
		"4294967299",           // truncates to 3 — looks like a deliberate position
		"8589934592",           // 2^33: also truncates to 0
		"9223372036854775808",  // MaxInt64 + 1
		"18446744073709551615", // MaxUint64
	} {
		got, err := parseIndex(in)
		if err == nil {
			t.Errorf("parseIndex(%q) = %d, nil; want an out-of-range error (this value truncates on 32-bit builds)", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("parseIndex(%q) error = %v; want it to say the index is out of range", in, err)
		}
	}
}

// TestParseIndexAcceptsRealPositions guards the other direction: the bound
// exists to make the int conversion total, not to second-guess the user, so
// everything representable must still go through unchanged.
func TestParseIndexAcceptsRealPositions(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"0", 0},
		{"1", 1},
		{"12", 12},
		{" 7 ", 7}, // parseUint trimmed; parseIndex must too
		{"2147483647", math.MaxInt32},
	} {
		got, err := parseIndex(tc.in)
		if err != nil {
			t.Errorf("parseIndex(%q) returned error %v; want %d", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("parseIndex(%q) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// TestParseIndexRejectsNonNumeric covers the inputs that were never the
// finding but share its consequence: parseUint's fatal() said only "bad id",
// which is the wrong noun for a position.
func TestParseIndexRejectsNonNumeric(t *testing.T) {
	for _, in := range []string{"", "abc", "-1", "1.5", "0x10", "3rd", "  "} {
		if got, err := parseIndex(in); err == nil {
			t.Errorf("parseIndex(%q) = %d, nil; want an error", in, got)
		}
	}
}

// TestParsePortRangeRejectsOutOfRange pins the second half of the fix. These
// used to survive the CLI and be narrowed by an unchecked uint16() in
// mesh.resolveLegs, a wire hop away where nothing could report the problem —
// so "-dport 80-100000" quietly became 80-34464.
func TestParsePortRangeRejectsOutOfRange(t *testing.T) {
	for _, in := range []string{
		"65536",     // one past the top: uint16() makes this 0
		"100000",    // becomes 34464
		"80-100000", // the realistic typo: a range whose top silently shrinks
		"65536-65540",
		"4294967296", // truncates twice over
	} {
		lo, hi, err := parsePortRange(in)
		if err == nil {
			t.Errorf("parsePortRange(%q) = %d, %d, nil; want an out-of-range error", in, lo, hi)
			continue
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("parsePortRange(%q) error = %v; want it to say the port is out of range", in, err)
		}
	}
}

// TestParsePortRangeRejectsNonNumeric is the more dangerous of the two old
// behaviours. Atoi's error was discarded, so "-dport http" parsed as 0,
// which cleared hasPorts in resolveLegs and left the rule with no port leg
// at all: a rule meant to deny one port denied every port, and its allow
// counterpart opened every port. Silently widening a firewall rule is not an
// acceptable response to a typo.
func TestParsePortRangeRejectsNonNumeric(t *testing.T) {
	for _, in := range []string{"http", "80,443", "8o", "80-", "-80", "--", "0x50"} {
		lo, hi, err := parsePortRange(in)
		if err == nil {
			t.Errorf("parsePortRange(%q) = %d, %d, nil; want an error (a rule with no port leg matches every port)", in, lo, hi)
		}
	}
}

// TestParsePortRangeAcceptsValid keeps the working cases working, including
// the one input that is still allowed to produce a zero range: empty, which
// is how "-sport"/"-dport" are left unset.
func TestParsePortRangeAcceptsValid(t *testing.T) {
	for _, tc := range []struct {
		in       string
		lo, hi   int
		whatItIs string
	}{
		{"", 0, 0, "unset — the only input that may still yield a zero range"},
		{"  ", 0, 0, "whitespace is unset too"},
		{"80", 80, 80, "single port"},
		{"80-443", 80, 443, "range"},
		{" 80 - 443 ", 80, 443, "range with padding around the separator"},
		{"65535", 65535, 65535, "the top of the range is inclusive"},
		{"1-65535", 1, 65535, "every port"},
		{"0", 0, 0, "0 means unset in the firewall's port fields, so it parses"},
		{"443-443", 443, 443, "degenerate range"},
	} {
		lo, hi, err := parsePortRange(tc.in)
		if err != nil {
			t.Errorf("parsePortRange(%q) returned error %v; want %d-%d (%s)", tc.in, err, tc.lo, tc.hi, tc.whatItIs)
			continue
		}
		if lo != tc.lo || hi != tc.hi {
			t.Errorf("parsePortRange(%q) = %d, %d; want %d, %d (%s)", tc.in, lo, hi, tc.lo, tc.hi, tc.whatItIs)
		}
	}
}

// TestParsePortRangeRejectsInverted is a behaviour change worth stating
// outright rather than leaving to be discovered: "-dport 443-80" used to be
// accepted and produced a leg matching nothing at all. The NAT translation
// path already rejected inverted ranges with its own check, so this only
// makes the two agree.
func TestParsePortRangeRejectsInverted(t *testing.T) {
	if lo, hi, err := parsePortRange("443-80"); err == nil {
		t.Errorf("parsePortRange(%q) = %d, %d, nil; want an error — an inverted range matches no port", "443-80", lo, hi)
	}
}
