package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestLearnPeersSeedsExtraUDPPorts: when gossip carries a not-yet-connected
// peer's extra UDP ports, each becomes its own seed candidate at the peer's
// address (alongside the endpoint itself) — reusing initLoop's existing
// retry/backoff/dedup rather than a separate dial mechanism, unlike the TCP
// side (ensureFallback), which has no equivalent existing pool to reuse.
func TestLearnPeersSeedsExtraUDPPorts(t *testing.T) {
	const netID = uint64(0xE47A)
	key, _ := crypto.GenerateKey()
	A := spinManaged(t, "A", netID, key, netip.MustParseAddr("10.9.2.1"), false, 0)
	B := spinManaged(t, "B", netID, key, netip.MustParseAddr("10.9.2.2"), false, 0)
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}
	ns := A.eng.network(netID)
	ns.mu.RLock()
	sessB := ns.byNode["B"]
	ns.mu.RUnlock()
	if sessB == nil {
		t.Fatal("no session to B")
	}

	// Gossip a not-yet-connected node C with extra UDP ports alongside its
	// primary endpoint. The primary port (9999, matching the endpoint) is
	// deliberately repeated in extraUDPPorts too, to confirm it's not
	// double-seeded.
	entries := []peerEntry{{
		nodeID:        "C",
		hostname:      "hc",
		overlay4:      netip.MustParseAddr("10.9.2.9"),
		endpoint:      netip.MustParseAddrPort("192.0.2.1:9999"),
		extraUDPPorts: []uint16{9999, 443, 80},
	}}
	A.eng.learnPeers(sessB, entries)

	want := map[netip.AddrPort]bool{
		netip.MustParseAddrPort("192.0.2.1:9999"): false, // the endpoint itself
		netip.MustParseAddrPort("192.0.2.1:443"):  false,
		netip.MustParseAddrPort("192.0.2.1:80"):   false,
	}
	ns.mu.RLock()
	seeds := append([]netip.AddrPort(nil), ns.seeds...)
	ns.mu.RUnlock()
	for _, s := range seeds {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for addr, got := range want {
		if !got {
			t.Errorf("expected %s to be seeded, wasn't; seeds: %v", addr, seeds)
		}
	}
	// 9999 should appear exactly once (as the endpoint), not a second time
	// duplicated from extraUDPPorts.
	count9999 := 0
	for _, s := range seeds {
		if s == netip.MustParseAddrPort("192.0.2.1:9999") {
			count9999++
		}
	}
	if count9999 != 1 {
		t.Errorf("expected 192.0.2.1:9999 seeded exactly once, got %d: %v", count9999, seeds)
	}
}
