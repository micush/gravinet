//go:build windows

package resolver

import "testing"

// TestPSQuoteArrayProducesArrayLiteralNotOneJoinedString locks in the fix for
// the bug where Add-DnsClientNrptRule -NameServers was given one single
// quoted, comma-joined string (a one-element array containing a malformed
// value) instead of a proper PowerShell array literal (comma-separated,
// individually-quoted elements). The NRPT policy store accepted the rule at
// creation but silently dropped the malformed entry on readback, so the
// domain showed up in the DNS state view with no servers.
func TestPSQuoteArrayProducesArrayLiteralNotOneJoinedString(t *testing.T) {
	got := psQuoteArray([]string{"10.0.0.1", "192.168.0.2"})
	want := "'10.0.0.1','192.168.0.2'"
	if got != want {
		t.Fatalf("psQuoteArray = %q, want %q (a PowerShell array literal, not one joined string)", got, want)
	}

	// The previous (buggy) behavior for comparison — this must NOT be what
	// we send to -NameServers.
	buggy := psQuote("10.0.0.1,192.168.0.2")
	if got == buggy {
		t.Fatalf("psQuoteArray must not match the old single-joined-string form %q", buggy)
	}
}

func TestPSQuoteArraySingleElement(t *testing.T) {
	if got, want := psQuoteArray([]string{"8.8.8.8"}), "'8.8.8.8'"; got != want {
		t.Fatalf("psQuoteArray = %q, want %q", got, want)
	}
}

func TestPSQuoteEscapesEmbeddedSingleQuote(t *testing.T) {
	if got, want := psQuote("it's"), "'it''s'"; got != want {
		t.Fatalf("psQuote = %q, want %q", got, want)
	}
}
