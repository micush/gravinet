package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestPeerListCarriesSeed: the gossip list carries per-entry seed status in
// its own trailing block, after the version block, and a list whose entries
// are all non-seeds omits the block entirely — mirroring
// TestPeerListCarriesVersion.
func TestPeerListCarriesSeed(t *testing.T) {
	in := []peerEntry{
		{nodeID: "A", hostname: "a", overlay4: netip.MustParseAddr("10.0.0.1"),
			endpoint: netip.MustParseAddrPort("198.51.100.7:65432"), tcpPort: 65432, selfSeed: true},
		{nodeID: "B", hostname: "b", overlay4: netip.MustParseAddr("10.0.0.2"),
			endpoint: netip.MustParseAddrPort("198.51.100.8:65432"), tcpPort: 8443, selfSeed: false},
	}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || !out[0].selfSeed || out[1].selfSeed {
		t.Fatalf("seed status not carried: %+v", out)
	}

	// No entry is a seed: the block is not emitted, so the encoding is
	// byte-identical to what it would have been before the field existed.
	none := []peerEntry{
		{nodeID: "A", hostname: "a", endpoint: netip.MustParseAddrPort("198.51.100.7:1"), tcpPort: 1},
	}
	withSeed := []peerEntry{
		{nodeID: "A", hostname: "a", endpoint: netip.MustParseAddrPort("198.51.100.7:1"), tcpPort: 1, selfSeed: true},
	}
	encNone, encSeed := encodePeerList(none), encodePeerList(withSeed)
	if len(encNone) != len(encSeed)-2 { // -1 marker byte, -1 per-entry seed byte
		t.Fatalf("no-seed encoding should omit the block: %d vs %d", len(encNone), len(encSeed))
	}
	backNone, err := decodePeerList(encNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(backNone) != 1 || backNone[0].selfSeed || backNone[0].tcpPort != 1 {
		t.Fatalf("no-seed round-trip wrong: %+v", backNone)
	}

	// An older decoder stops at the unrecognized seed block; everything
	// before it (including the version block, if any) must still decode.
	trimmed := encSeed[:len(encSeed)-2]
	backOld, err := decodePeerList(trimmed)
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if len(backOld) != 1 || backOld[0].selfSeed || backOld[0].tcpPort != 1 {
		t.Fatalf("backward-compat wrong: %+v", backOld)
	}
}

// pmNode is a minimal engine+transport pair for the partial-mesh handshake
// tests below — same shape as TestSelfSeedDoesNotDegradeOtherPeers' local
// mk() closure (not spinNode/ban_test.go's, which has no way to set
// SelfSeed/PartialMesh on the NetSpec).
type pmNode struct {
	eng *Engine
	tr  *transport.Transport
	dev *fakeDev
}

func spinPMNode(t *testing.T, name string, netID uint64, ks *crypto.KeySet, self netip.Addr, selfSeed, partialMesh bool) *pmNode {
	t.Helper()
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, SelfSeed: selfSeed, PartialMesh: partialMesh}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &pmNode{eng, tr, dev}
}

func (n *pmNode) close() {
	n.dev.Close()
	n.eng.Stop()
	n.tr.Close()
}

func (n *pmNode) addr() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(n.tr.Port()))
}

// TestPartialMeshPeerToPeerRejected is the core guarantee: on a partial-mesh
// network, two nodes that are neither of them a seed must never end up
// connected to each other, even when each is explicitly told the other's
// address (e.g. an operator mistakenly listing a peer in Seeds) — the
// handshake itself refuses to complete, on both the accepting side
// (onHSInit) and the dialing side (onHSResp), regardless of which one
// initiates. Both directions are exercised here by seeding both ends.
func TestPartialMeshPeerToPeerRejected(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatal(err)
	}
	const netID = uint64(0xFEED0001)

	P1 := spinPMNode(t, "p1", netID, ks, netip.MustParseAddr("10.60.0.1"), false, true)
	P2 := spinPMNode(t, "p2", netID, ks, netip.MustParseAddr("10.60.0.2"), false, true)
	defer P1.close()
	defer P2.close()

	P1.eng.AddSeed(netID, P2.addr())
	P2.eng.AddSeed(netID, P1.addr())

	// Give the retry/backoff loop several real rounds to attempt and be
	// refused, then confirm neither ever ends up with a session — a
	// transient false positive here would be exactly the bug this test
	// exists to catch, so this checks steady-state, not just t=0.
	time.Sleep(3 * time.Second)
	if n := P1.eng.PeerCount(netID); n != 0 {
		t.Fatalf("P1 has %d peers, want 0 — peer-to-peer link formed on a partial mesh network", n)
	}
	if n := P2.eng.PeerCount(netID); n != 0 {
		t.Fatalf("P2 has %d peers, want 0 — peer-to-peer link formed on a partial mesh network", n)
	}
}

// TestPartialMeshSeedLinksAllowed confirms the two permitted link types both
// still form normally: seed<->peer (dialed from the peer's side, exercising
// onHSInit's seed check on the seed and onHSResp's on the peer) and
// seed<->seed. A partial mesh that blocked these too would just be a broken
// full mesh, not a restricted one.
func TestPartialMeshSeedLinksAllowed(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatal(err)
	}
	const netID = uint64(0xFEED0002)

	S1 := spinPMNode(t, "s1", netID, ks, netip.MustParseAddr("10.60.1.1"), true, true)
	S2 := spinPMNode(t, "s2", netID, ks, netip.MustParseAddr("10.60.1.2"), true, true)
	P := spinPMNode(t, "peer", netID, ks, netip.MustParseAddr("10.60.1.3"), false, true)
	defer S1.close()
	defer S2.close()
	defer P.close()

	// Seed <-> seed.
	S1.eng.AddSeed(netID, S2.addr())
	S2.eng.AddSeed(netID, S1.addr())
	// Peer -> seed (the peer dials; the seed never dials the peer, matching
	// how a partial mesh's own gossip auto-dial behaves — see
	// TestPartialMeshGossipSkipsPeerToPeerAutoDial).
	P.eng.AddSeed(netID, S1.addr())

	if !waitUntil(10*time.Second, func() bool {
		return S1.eng.PeerCount(netID) == 2 && S2.eng.PeerCount(netID) == 1 && P.eng.PeerCount(netID) == 1
	}) {
		t.Fatalf("seed links did not form: S1=%d S2=%d P=%d",
			S1.eng.PeerCount(netID), S2.eng.PeerCount(netID), P.eng.PeerCount(netID))
	}
}

// TestPartialMeshGossipSkipsPeerToPeerAutoDial checks the courtesy half of
// the feature, not just the hard-enforcement half: on a partial mesh, a peer
// that learns about another peer purely through a seed's gossip must not
// even attempt to auto-dial it (learnPeers' PartialMesh gate), even though
// the handshake would refuse the attempt anyway if it tried. This is also
// what actually exercises the gossiped selfSeed bit end-to-end (wire
// encode/decode plus learnPeers' propagation into nodeInfo), not just the
// direct-handshake case TestPartialMeshSeedLinksAllowed covers.
func TestPartialMeshGossipSkipsPeerToPeerAutoDial(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatal(err)
	}
	const netID = uint64(0xFEED0003)

	S := spinPMNode(t, "seed", netID, ks, netip.MustParseAddr("10.60.2.1"), true, true)
	P1 := spinPMNode(t, "p1", netID, ks, netip.MustParseAddr("10.60.2.2"), false, true)
	P2 := spinPMNode(t, "p2", netID, ks, netip.MustParseAddr("10.60.2.3"), false, true)
	defer S.close()
	defer P1.close()
	defer P2.close()

	P1.eng.AddSeed(netID, S.addr())
	P2.eng.AddSeed(netID, S.addr())

	if !waitUntil(10*time.Second, func() bool {
		return S.eng.PeerCount(netID) == 2 && P1.eng.PeerCount(netID) == 1 && P2.eng.PeerCount(netID) == 1
	}) {
		t.Fatalf("peers did not both connect to the seed: S=%d P1=%d P2=%d",
			S.eng.PeerCount(netID), P1.eng.PeerCount(netID), P2.eng.PeerCount(netID))
	}

	// Let several gossip rounds pass so each peer hears about the other via
	// S — plenty of opportunity to wrongly auto-dial if the PartialMesh
	// gate in learnPeers were missing or the gossiped selfSeed bit wrong.
	time.Sleep(3 * time.Second)
	if n := P1.eng.PeerCount(netID); n != 1 {
		t.Fatalf("P1 has %d peers, want 1 (S only) — gossip about P2 triggered an auto-dial it shouldn't have", n)
	}
	if n := P2.eng.PeerCount(netID); n != 1 {
		t.Fatalf("P2 has %d peers, want 1 (S only) — gossip about P1 triggered an auto-dial it shouldn't have", n)
	}
}
