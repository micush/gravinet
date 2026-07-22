package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func spinManaged(t *testing.T, name string, netID uint64, key string, self netip.Addr, managed bool, webPort uint16) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name, Managed: managed, WebPort: webPort,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}},
	})
	tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

// TestManagedAdvertisement: a managed node is discovered by peers with its web
// port; a non-managed node is not; and the entry ages out of the TTL window.
func TestManagedAdvertisement(t *testing.T) {
	const netID = uint64(0x3210)
	key, _ := crypto.GenerateKey()
	A := spinManaged(t, "A", netID, key, netip.MustParseAddr("10.8.0.1"), false, 0)
	B := spinManaged(t, "B", netID, key, netip.MustParseAddr("10.8.0.2"), true, 8443)
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	A.eng.AddSeed(netID, port(B))
	B.eng.AddSeed(netID, port(A))

	if !waitUntil(8*time.Second, func() bool {
		mp := A.eng.ManagedPeers(time.Minute)
		return len(mp) == 1 && mp[0].NodeID == "B" && mp[0].WebPort == 8443
	}) {
		t.Fatalf("A did not discover B as a managed peer with web port; got %+v", A.eng.ManagedPeers(time.Minute))
	}
	// B advertised managed; A did not, so B must not see A as managed.
	if mp := B.eng.ManagedPeers(time.Minute); len(mp) != 0 {
		t.Fatalf("B should not see non-managed A; got %+v", mp)
	}
	// A reaches B on its overlay address.
	if !A.eng.IsOverlayAddr(netip.MustParseAddr("10.8.0.2")) {
		t.Fatal("A should recognize B's overlay address")
	}
	// TTL filtering exempts a currently-connected peer entirely: an active
	// session is a definitive liveness signal, and ni.lastSeen for a connected
	// peer isn't kept fresh by anything ongoing (no periodic touch from
	// ping/pong) — only by a fresh handshake or third-party gossip mentioning
	// it — so applying the TTL to a connected peer flickered it in and out of
	// this list on whatever cadence gossip happened to (not) arrive. B is
	// still connected here, so even a vanishingly tiny TTL must not drop it.
	if mp := A.eng.ManagedPeers(time.Nanosecond); len(mp) != 1 || mp[0].NodeID != "B" {
		t.Fatalf("a tiny TTL must not drop a still-connected managed peer; got %+v", mp)
	}
	// A gossip-only (not connected) peer is a different story: it has no live
	// session to fall back on, so the TTL is the only staleness signal there
	// is, and it must still apply. C is never seeded/dialed, only learned via
	// a synthetic gossip entry relayed through the real, already-established
	// session to B (mirroring TestManagerLearnedViaGossipOnly's pattern).
	ns := A.eng.network(netID)
	ns.mu.RLock()
	sessB := ns.byNode["B"]
	ns.mu.RUnlock()
	if sessB == nil {
		t.Fatal("no session to B")
	}
	A.eng.learnPeers(sessB, []peerEntry{{
		nodeID: "C", hostname: "hc",
		overlay4: netip.MustParseAddr("10.8.0.9"),
		endpoint: netip.MustParseAddrPort("192.0.2.1:9999"),
		managed:  true, webPort: 9443,
	}})
	if mp := A.eng.ManagedPeers(time.Minute); len(mp) != 2 {
		t.Fatalf("A should now know both connected B and gossip-only C; got %+v", mp)
	}
	if mp := A.eng.ManagedPeers(time.Nanosecond); len(mp) != 1 || mp[0].NodeID != "B" {
		t.Fatalf("tiny TTL should drop gossip-only C but keep connected B; got %+v", mp)
	}
}
