package main

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
)

func TestResolveSeedsPortExpansion(t *testing.T) {
	want := 1 + len(config.FallbackUDPPorts) // primary + fallbacks

	// Bare IPv4 → tried on every standard port.
	if got := resolveSeeds([]string{"198.51.100.7"}, config.DefaultUDPPort, nil); len(got) != want {
		t.Errorf("bare IPv4: got %d endpoints, want %d", len(got), want)
	}

	// Bare IPv6 literal → also expanded.
	if got := resolveSeeds([]string{"2001:db8::5"}, config.DefaultUDPPort, nil); len(got) != want {
		t.Errorf("bare IPv6: got %d endpoints, want %d", len(got), want)
	}

	// Explicit port → used verbatim, exactly one endpoint.
	got := resolveSeeds([]string{"203.0.113.9:60000"}, config.DefaultUDPPort, nil)
	if len(got) != 1 {
		t.Fatalf("explicit port: got %d endpoints, want 1", len(got))
	}
	if got[0].Port() != 60000 {
		t.Errorf("explicit port: got port %d, want 60000", got[0].Port())
	}

	// A non-default primary is included in the expansion.
	got = resolveSeeds([]string{"198.51.100.7"}, 7777, nil)
	found := false
	for _, ap := range got {
		if ap.Port() == 7777 {
			found = true
		}
	}
	if !found {
		t.Error("custom primary port 7777 not present in expanded seeds")
	}

	// Duplicates collapse: same host:port given twice → one endpoint.
	if got := resolveSeeds([]string{"198.51.100.7:443", "198.51.100.7:443"}, config.DefaultUDPPort, nil); len(got) != 1 {
		t.Errorf("dedup: got %d endpoints, want 1", len(got))
	}
}

func TestResolveSeedsRejectsOverlayAddress(t *testing.T) {
	overlay := netip.MustParsePrefix("10.77.0.0/16")
	// A seed that resolves to an address inside our overlay subnet must be
	// dropped — it would be a stale tunnel IP that can't be used to bootstrap.
	got := resolveSeeds([]string{"10.77.42.9:51820"}, config.DefaultUDPPort, []netip.Prefix{overlay})
	if len(got) != 0 {
		t.Fatalf("expected overlay seed to be rejected, got %v", got)
	}
	// An underlay address with the same overlays present is still accepted.
	got = resolveSeeds([]string{"198.51.100.7:51820"}, config.DefaultUDPPort, []netip.Prefix{overlay})
	if len(got) != 1 {
		t.Fatalf("expected underlay seed accepted, got %v", got)
	}
}

// TestResolveSeedsMultiPort covers "host:port,port,..." — an operator-chosen
// port list, distinct from the no-port case's built-in fallback expansion
// (TestResolveSeedsPortExpansion above).
func TestResolveSeedsMultiPort(t *testing.T) {
	got := resolveSeeds([]string{"203.0.113.9:60000,60001,60002"}, config.DefaultUDPPort, nil)
	if len(got) != 3 {
		t.Fatalf("multi-port: got %d endpoints, want 3: %v", len(got), got)
	}
	wantPorts := map[uint16]bool{60000: true, 60001: true, 60002: true}
	for _, ap := range got {
		if !wantPorts[ap.Port()] {
			t.Errorf("unexpected port %d in %v", ap.Port(), got)
		}
		delete(wantPorts, ap.Port())
	}
	if len(wantPorts) != 0 {
		t.Errorf("missing ports %v in %v", wantPorts, got)
	}

	// A single explicit port keeps behaving exactly as before — no comma,
	// same one-endpoint result as TestResolveSeedsPortExpansion's own check.
	if got := resolveSeeds([]string{"203.0.113.9:60000"}, config.DefaultUDPPort, nil); len(got) != 1 {
		t.Errorf("single port regressed: got %d endpoints, want 1", len(got))
	}

	// Duplicates within one seed's port list collapse the same way separate
	// seeds do.
	if got := resolveSeeds([]string{"198.51.100.7:443,443"}, config.DefaultUDPPort, nil); len(got) != 1 {
		t.Errorf("dedup within a multi-port list: got %d endpoints, want 1", len(got))
	}
}
