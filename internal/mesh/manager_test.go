package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// spinManagerNode is spinManaged's counterpart with independent control over
// both Managed and Manager, for exercising the split rather than just one
// flag at a time.
func spinManagerNode(t *testing.T, name string, netID uint64, key string, self netip.Addr, managed, manager bool, webPort uint16) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name, Managed: managed, Manager: manager, WebPort: webPort,
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

// TestPeerListCarriesManager mirrors TestPeerListCarriesManaged: the manager
// bit survives a gossip encode/decode round-trip, independently of managed —
// so a Manager-but-not-Managed (or vice versa) peer propagates correctly.
func TestPeerListCarriesManager(t *testing.T) {
	entries := []peerEntry{
		{nodeID: "X", hostname: "hx", overlay4: netip.MustParseAddr("10.0.0.5"),
			endpoint: netip.MustParseAddrPort("1.2.3.4:5"), managed: false, manager: true},
		{nodeID: "Y", hostname: "hy", overlay4: netip.MustParseAddr("10.0.0.6"),
			endpoint: netip.MustParseAddrPort("1.2.3.4:6"), managed: true, manager: false, webPort: 8443},
	}
	dec, err := decodePeerList(encodePeerList(entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != 2 {
		t.Fatalf("got %d entries, want 2", len(dec))
	}
	var x, y *peerEntry
	for i := range dec {
		switch dec[i].nodeID {
		case "X":
			x = &dec[i]
		case "Y":
			y = &dec[i]
		}
	}
	if x == nil || !x.manager || x.managed {
		t.Fatalf("X should be manager-only (manager=true, managed=false): %+v", x)
	}
	if y == nil || y.manager || !y.managed || y.webPort != 8443 {
		t.Fatalf("Y should be managed-only (managed=true, manager=false), webPort intact: %+v", y)
	}
}

// TestManagerIndependentOfManaged spins up a node that is Manager but not
// Managed, and one that is Managed but not Manager, and checks that
// ManagedPeers() (the "can be managed" listing) and IsManagerAddr() (the "can
// manage others" check) track their own flag, not each other's.
func TestManagerIndependentOfManaged(t *testing.T) {
	const netID = uint64(0x4442)
	key, _ := crypto.GenerateKey()
	// A: manager-only (a bastion/admin-console node — can manage, can't be managed).
	// B: managed-only (manageable, but can't itself manage anyone).
	A := spinManagerNode(t, "A", netID, key, netip.MustParseAddr("10.44.0.1"), false, true, 0)
	B := spinManagerNode(t, "B", netID, key, netip.MustParseAddr("10.44.0.2"), true, false, 8443)
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

	// A should discover B as managed (B advertised Managed).
	if !waitUntil(8*time.Second, func() bool {
		mp := A.eng.ManagedPeers(time.Minute)
		return len(mp) == 1 && mp[0].NodeID == "B" && mp[0].WebPort == 8443
	}) {
		t.Fatalf("A did not discover B as managed; got %+v", A.eng.ManagedPeers(time.Minute))
	}
	// B should NOT see A as managed — A never advertised Managed, only Manager.
	if mp := B.eng.ManagedPeers(time.Minute); len(mp) != 0 {
		t.Fatalf("B should not see manager-only A as managed; got %+v", mp)
	}
	// B should recognize A's address as belonging to a manager.
	if !waitUntil(8*time.Second, func() bool {
		return B.eng.IsManagerAddr(netip.MustParseAddr("10.44.0.1"))
	}) {
		t.Fatal("B should recognize A as a manager (A advertised Manager mode)")
	}
	// A should NOT recognize B's address as a manager — B never advertised Manager.
	if A.eng.IsManagerAddr(netip.MustParseAddr("10.44.0.2")) {
		t.Fatal("A should not treat managed-only B as a manager")
	}
}

// TestLiveToggleManagerPropagatesToConnectedPeer is the exact scenario a live
// report hit: two nodes are already meshed (both start with Managed=Manager=
// false — nothing to advertise yet), then one flips Manager on live via
// SetManager (what the web UI's toggle and "gravinet manager on" both call)
// while the session is still up. The other node must recognize the new
// Manager status immediately — not only after some future reconnect — since
// that's what the whole live-toggle promise ("applies now, nothing to
// restart") means in practice, and because IsManagerAddr on the *peer* being
// managed is exactly what authorizes the proxied management request in the
// first place: without the immediate push, enabling Manager and trying to use
// it right away against an already-connected peer would 401.
func TestLiveToggleManagerPropagatesToConnectedPeer(t *testing.T) {
	const netID = uint64(0x4443)
	key, _ := crypto.GenerateKey()
	A := spinManagerNode(t, "A", netID, key, netip.MustParseAddr("10.44.1.1"), false, false, 0)
	B := spinManagerNode(t, "B", netID, key, netip.MustParseAddr("10.44.1.2"), true, false, 8443)
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

	// Get the session up first, with A not yet a manager.
	if !waitUntil(8*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}
	if B.eng.IsManagerAddr(netip.MustParseAddr("10.44.1.1")) {
		t.Fatal("B should not see A as a manager yet — A hasn't enabled Manager mode")
	}

	// Flip Manager on live on the already-connected A — no reconnect, no restart.
	A.eng.SetManager(true)

	// B (already connected to A the whole time) must pick this up without a
	// reconnect: this is precisely what announceClusterState/ctrlClusterNotify
	// exists for.
	if !waitUntil(5*time.Second, func() bool {
		return B.eng.IsManagerAddr(netip.MustParseAddr("10.44.1.1"))
	}) {
		t.Fatal("B did not learn A's live Manager toggle without a reconnect")
	}

	// And flipping it back off live must also propagate.
	A.eng.SetManager(false)
	if !waitUntil(5*time.Second, func() bool {
		return !B.eng.IsManagerAddr(netip.MustParseAddr("10.44.1.1"))
	}) {
		t.Fatal("B did not learn A's live Manager-off toggle without a reconnect")
	}
}

// TestLiveToggleManagedPropagatesToConnectedPeer is TestLiveToggleManager...'s
// counterpart for Managed: flipping Managed live on an already-connected peer
// must update the other side's ManagedPeers() listing without a reconnect,
// the same way Manager does.
func TestLiveToggleManagedPropagatesToConnectedPeer(t *testing.T) {
	const netID = uint64(0x4444)
	key, _ := crypto.GenerateKey()
	A := spinManagerNode(t, "A", netID, key, netip.MustParseAddr("10.44.2.1"), false, false, 0)
	B := spinManagerNode(t, "B", netID, key, netip.MustParseAddr("10.44.2.2"), false, false, 0)
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

	if !waitUntil(8*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}
	if mp := B.eng.ManagedPeers(time.Minute); len(mp) != 0 {
		t.Fatalf("B should not see A as managed yet; got %+v", mp)
	}

	A.eng.SetManaged(true)

	if !waitUntil(5*time.Second, func() bool {
		mp := B.eng.ManagedPeers(time.Minute)
		return len(mp) == 1 && mp[0].NodeID == "A"
	}) {
		t.Fatal("B did not learn A's live Managed toggle without a reconnect")
	}
}

// TestManagerLearnedViaGossipOnly mirrors TestManagedLearnedViaGossipOnly: a
// node C advertising Manager (but unreachable directly, so only known via
// gossip) must still resolve via IsManagerAddr on a node that only heard about
// it through a relay — otherwise multi-hop management-initiation would break
// exactly like multi-hop being-managed did before managed rode gossip.
func TestManagerLearnedViaGossipOnly(t *testing.T) {
	const netID = uint64(0x90552)
	key, _ := crypto.GenerateKey()
	A := spinManagerNode(t, "A", netID, key, netip.MustParseAddr("10.9.2.1"), false, false, 0)
	B := spinManagerNode(t, "B", netID, key, netip.MustParseAddr("10.9.2.2"), false, false, 0)
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

	// Gossip advertising manager node C, which A is not connected to; the
	// endpoint is unroutable (TEST-NET-1) so A can't form a direct session.
	entries := []peerEntry{{
		nodeID: "C", hostname: "hc",
		overlay4: netip.MustParseAddr("10.9.2.9"),
		endpoint: netip.MustParseAddrPort("192.0.2.1:9999"),
		manager:  true,
	}}
	A.eng.learnPeers(sessB, entries)

	if !A.eng.IsManagerAddr(netip.MustParseAddr("10.9.2.9")) {
		t.Fatal("A did not learn manager C from gossip")
	}
	ns.mu.RLock()
	_, direct := ns.byNode["C"]
	ns.mu.RUnlock()
	if direct {
		t.Fatal("test invalid: A formed a direct session to C (should be gossip-only)")
	}
}
