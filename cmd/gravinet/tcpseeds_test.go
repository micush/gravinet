package main

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
)

// TestResolveTCPSeedsBasics covers resolveTCPSeeds' pre-existing behavior —
// no dedicated test previously existed for it, so this pins down the
// no-port-uses-default-fallback-port and explicit-port-used-verbatim cases
// alongside the new multi-port one below, the same split
// TestResolveSeedsPortExpansion uses for the UDP side.
func TestResolveTCPSeedsBasics(t *testing.T) {
	// Non-tcp seeds are ignored entirely (handled by resolveSeeds instead).
	if got := resolveTCPSeeds([]string{"203.0.113.9:60000"}, config.DefaultTCPFallbackPort, nil); len(got) != 0 {
		t.Errorf("bare udp-style seed leaked into TCP resolution: %v", got)
	}

	// No port → the default TCP fallback port.
	got := resolveTCPSeeds([]string{"tcp://203.0.113.9"}, 7777, nil)
	if len(got) != 1 || got[0].Port() != 7777 {
		t.Fatalf("no-port tcp seed: got %v, want one endpoint on port 7777", got)
	}

	// Explicit single port used verbatim.
	got = resolveTCPSeeds([]string{"tcp://203.0.113.9:60000"}, config.DefaultTCPFallbackPort, nil)
	if len(got) != 1 || got[0].Port() != 60000 {
		t.Fatalf("explicit tcp port: got %v, want one endpoint on port 60000", got)
	}

	// Duplicates collapse.
	if got := resolveTCPSeeds([]string{"tcp://203.0.113.9:443", "tcp://203.0.113.9:443"}, config.DefaultTCPFallbackPort, nil); len(got) != 1 {
		t.Errorf("dedup: got %d endpoints, want 1", len(got))
	}
}

// TestResolveTCPSeedsMultiPort covers "tcp://host:port,port,..." — same
// operator-chosen-list idea as the UDP side's TestResolveSeedsMultiPort.
func TestResolveTCPSeedsMultiPort(t *testing.T) {
	got := resolveTCPSeeds([]string{"tcp://203.0.113.9:60000,60001,60002"}, config.DefaultTCPFallbackPort, nil)
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
}

// TestResolveTCPSeedsRejectsOverlayAddress mirrors the UDP side's inception
// guard test — a tcp seed resolving inside an overlay subnet must be dropped.
func TestResolveTCPSeedsRejectsOverlayAddress(t *testing.T) {
	overlay := netip.MustParsePrefix("10.77.0.0/16")
	got := resolveTCPSeeds([]string{"tcp://10.77.42.9:51820"}, config.DefaultTCPFallbackPort, []netip.Prefix{overlay})
	if len(got) != 0 {
		t.Fatalf("expected overlay tcp seed to be rejected, got %v", got)
	}
	got = resolveTCPSeeds([]string{"tcp://198.51.100.7:51820"}, config.DefaultTCPFallbackPort, []netip.Prefix{overlay})
	if len(got) != 1 {
		t.Fatalf("expected underlay tcp seed accepted, got %v", got)
	}
}
