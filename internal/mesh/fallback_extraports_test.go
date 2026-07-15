package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestEnsureFallbackDialsExtraPortsInParallel: when a peer advertises extra
// TCP ports alongside its primary one (via gossip/handshake-learned node
// info, same as TestEnsureFallbackUsesAdvertisedPort's single-port case),
// ensureFallback dials the primary *and* every extra port, not just one —
// and all as part of the same call, not one after another waiting on a
// timeout, which is the whole point (see ensureFallback's own doc comment).
func TestEnsureFallbackDialsExtraPortsInParallel(t *testing.T) {
	e, f, ns := fallbackEngine(t, 65432) // our own port is 65432
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.Lock()
	ns.nodes["peerX"] = &nodeInfo{
		nodeID: "peerX", endpoint: seed, tcpPort: 8443,
		extraTCPPorts: []uint16{443, 80},
		lastSeen:      time.Now(),
	}
	ns.mu.Unlock()

	e.ensureFallback(ns, seed)
	want := map[netip.AddrPort]bool{
		netip.MustParseAddrPort("203.0.113.7:8443"): false,
		netip.MustParseAddrPort("203.0.113.7:443"):  false,
		netip.MustParseAddrPort("203.0.113.7:80"):   false,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allDialed := true
		for _, d := range f.dials() {
			if _, ok := want[d]; ok {
				want[d] = true
			}
		}
		for _, got := range want {
			if !got {
				allDialed = false
			}
		}
		if allDialed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for addr, got := range want {
		if !got {
			t.Errorf("expected a dial to %s, never happened (dialed: %v)", addr, f.dials())
		}
	}
	if got := len(f.dials()); got != 3 {
		t.Errorf("expected exactly 3 dials (primary + 2 extras), got %d: %v", got, f.dials())
	}
}

// TestEnsureFallbackSkipsExtraPortDuplicatingPrimary confirms an extra port
// that happens to equal the resolved primary isn't dialed twice.
func TestEnsureFallbackSkipsExtraPortDuplicatingPrimary(t *testing.T) {
	e, f, ns := fallbackEngine(t, 65432)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.Lock()
	ns.nodes["peerX"] = &nodeInfo{
		nodeID: "peerX", endpoint: seed, tcpPort: 8443,
		extraTCPPorts: []uint16{8443, 443}, // 8443 duplicates the primary
		lastSeen:      time.Now(),
	}
	ns.mu.Unlock()

	e.ensureFallback(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(f.dials()); got != 2 {
		t.Fatalf("expected exactly 2 dials (8443 once, 443 once), got %d: %v", got, f.dials())
	}
}
