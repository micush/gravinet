package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestEnsureTCPDialsExtraPortsInParallel: when a peer advertises extra
// TCP ports alongside its primary one (via gossip/handshake-learned node
// info, same as TestEnsureTCPUsesAdvertisedPort's single-port case),
// ensureTCP dials the primary *and* every extra port, not just one —
// and all as part of the same call, not one after another waiting on a
// timeout, which is the whole point (see ensureTCP's own doc comment).
func TestEnsureTCPDialsExtraPortsInParallel(t *testing.T) {
	e, f, ns := tcpEngine(t, 65432) // our own port is 65432
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.Lock()
	ns.nodes["peerX"] = &nodeInfo{
		nodeID: "peerX", endpoint: seed, tcpPort: 8443,
		extraTCPPorts: []uint16{443, 80},
		lastSeen:      time.Now(),
	}
	ns.mu.Unlock()

	e.ensureTCP(ns, seed)
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
	// Each candidate dialed at most once; the count itself is not pinned,
	// since the seed's own port is a candidate too under the flat model.
	assertNoDuplicateDials(t, f)
}

// TestEnsureTCPSkipsExtraPortDuplicatingPrimary confirms an extra port
// that happens to equal the resolved primary isn't dialed twice.
func TestEnsureTCPSkipsExtraPortDuplicatingPrimary(t *testing.T) {
	e, f, ns := tcpEngine(t, 65432)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.Lock()
	ns.nodes["peerX"] = &nodeInfo{
		nodeID: "peerX", endpoint: seed, tcpPort: 8443,
		extraTCPPorts: []uint16{8443, 443}, // 8443 duplicates the primary
		lastSeen:      time.Now(),
	}
	ns.mu.Unlock()

	e.ensureTCP(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	// The property under test is that a duplicate extra port isn't dialed
	// twice — not the total, which now includes the seed's own port.
	assertNoDuplicateDials(t, f)
	if !dialedContains(f, netip.MustParseAddrPort("203.0.113.7:8443")) ||
		!dialedContains(f, netip.MustParseAddrPort("203.0.113.7:443")) {
		t.Fatalf("expected dials to both 8443 and 443, got %v", f.dials())
	}
}

// assertNoDuplicateDials pins the deduplication that matters: whatever the
// candidate set contains, no address in it is dialed more than once.
func assertNoDuplicateDials(t *testing.T, f *fakeTCP) {
	t.Helper()
	seen := map[netip.AddrPort]int{}
	for _, d := range f.dials() {
		seen[d]++
	}
	for addr, n := range seen {
		if n != 1 {
			t.Errorf("address %s dialed %d times, want once", addr, n)
		}
	}
}
