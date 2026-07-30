package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// Real handshake over real loopback sockets (same spinNode pattern as
// TestOutboundHandshakeAttributesConfiguredSeedOwner): B declares itself a
// seed via ns.spec.SelfSeed, A connects to B with no seed configuration at
// all, and A's ManagedPeers should still recognize B as a seed purely from
// what B advertised in the handshake -- proving the full path end to end,
// not just the wire encode/decode or the ManagedPeers consumption in
// isolation.
func TestManagedPeersRecognizesSelfSeedFromRealHandshake(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x5E1F5EED)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.2.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.2.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	// B declares itself a seed for this network -- set directly on its own
	// live spec, the same field cmd/gravinet populates from config.Network.
	// SelfSeed at startup. A has no seed configuration for B at all: no
	// configuredSeeds, no AddSeed, nothing address-based to go on.
	bns := B.eng.netSnapshot()[netID]
	bns.mu.Lock()
	bns.spec.SelfSeed = true
	bns.mu.Unlock()
	// Managed is a separate concept from SelfSeed (opts into remote
	// management/upgrades vs. "treat me as a seed") -- but ManagedPeers
	// filters to managed peers only, matching the real scenario this feature
	// is for: an operator's own managed fleet. Without this, B wouldn't show
	// up in A.ManagedPeers at all, regardless of SelfSeed.
	B.eng.SetManaged(true)

	seed := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(B.tr.Port()))
	A.eng.AddSeed(netID, seed)

	if !waitUntil(8*time.Second, func() bool { return len(A.eng.PeerEndpoints(netID)) > 0 }) {
		t.Fatal("A never connected to B via the seed")
	}

	peers := A.eng.ManagedPeers(0)
	var found *ManagedPeer
	for i := range peers {
		if peers[i].NodeID == "B" {
			found = &peers[i]
		}
	}
	if found == nil {
		t.Fatal("B not present in A's ManagedPeers at all")
	}
	if !found.IsSeed {
		t.Error("B declared SelfSeed=true over the handshake, but A.ManagedPeers doesn't reflect it")
	}
}

// ni.selfSeed (the operator's explicit "I am a seed" declaration, advertised
// via hsPayload.SelfSeed) is authoritative the moment it's set -- true even
// for a node with no endpoint anywhere near a configured seed address and no
// seedOwner attribution at all, the exact situation address-based inference
// can never resolve (see TestManagedPeersIsSeedResolvesTwoSeedsSharingOneAddressByTransport's
// predecessor, which this toggle was added specifically to have an escape
// hatch for).
func TestManagedPeersIsSeedTrueViaSelfSeedRegardlessOfAddress(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	// No configuredSeeds, no configuredTCPSeeds, no seedOwner entries at all --
	// nothing for any address-based check to find.
	addManagedNode(ns, "a", netip.MustParseAddrPort("192.0.2.1:41000"))
	ns.nodes["a"].selfSeed = true

	peers := e.ManagedPeers(0)
	if len(peers) != 1 || !peers[0].IsSeed {
		t.Errorf("node with selfSeed=true and no address-based match at all: IsSeed = %v, want true", len(peers) == 1 && peers[0].IsSeed)
	}
}

// selfSeed=false on an otherwise-ordinary node must not itself cause a false
// positive or suppress a real address-based match found some other way --
// it's an additional signal, never a veto.
func TestManagedPeersIsSeedFalseSelfSeedDoesNotVetoAddressMatch(t *testing.T) {
	seedAddr := netip.MustParseAddrPort("203.0.113.9:65432")

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{seedAddr}
	addManagedNode(ns, "a", seedAddr) // selfSeed left at its zero value (false)

	peers := e.ManagedPeers(0)
	if len(peers) != 1 || !peers[0].IsSeed {
		t.Errorf("node at the seed's address with selfSeed=false: IsSeed = %v, want true (address match should still count)", len(peers) == 1 && peers[0].IsSeed)
	}
}

// addManagedNode installs a bare-minimum managed, currently-connected node
// directly into ns's registry -- no real handshake, no real session, just
// enough for ManagedPeers to see it. byNode only needs a truthy presence to
// read as "connected" (ManagedPeers keys off _, connected := ns.byNode[id]),
// so an empty *peerSession stub is enough; nothing here ever calls a method
// on it.
func addManagedNode(ns *netState, nodeID string, endpoint netip.AddrPort) {
	ns.nodes[nodeID] = &nodeInfo{
		nodeID:   nodeID,
		hostname: nodeID,
		managed:  true,
		endpoint: endpoint,
		lastSeen: time.Now(),
	}
	ns.byNode[nodeID] = &peerSession{net: ns, nodeID: nodeID}
}

// This drives a genuine handshake over real loopback sockets (same pattern
// as TestOutboundHandshakeAttributesSeedOwner, which proves the analogous
// thing for the older, address-only ns.seedOwner) to prove the actual
// onHSResp write path -- not just ManagedPeers' read side, which every test
// above this one exercises by directly seeding configuredSeedOwnerUDP/TCP
// rather than earning it through a real handshake. If the membership check
// added there, or the e.tr/fallbackDialer transport detection, or the
// locking around either, were subtly wrong, a test that only pokes the map
// directly would never catch it.
func TestOutboundHandshakeAttributesConfiguredSeedOwner(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xC0FEE5)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.1.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.1.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	seed := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(B.tr.Port()))

	// configuredSeeds isn't something AddSeed touches (deliberately -- see
	// its doc comment on NetSpec.ConfiguredSeeds: it's a reload/construction-
	// time snapshot of the operator's actual config, not a live-growing dial
	// set), so it's set directly here, same as every other test in this
	// file. AddSeed is what actually makes A dial B over UDP.
	ns := A.eng.netSnapshot()[netID]
	ns.mu.Lock()
	ns.configuredSeeds = []netip.AddrPort{seed}
	ns.mu.Unlock()
	A.eng.AddSeed(netID, seed)

	if !waitUntil(8*time.Second, func() bool { return len(A.eng.PeerEndpoints(netID)) > 0 }) {
		t.Fatal("A never connected to B via the seed")
	}

	ns.mu.RLock()
	udpOwner := ns.configuredSeedOwnerUDP[seed]
	tcpOwner := ns.configuredSeedOwnerTCP[seed]
	ns.mu.RUnlock()
	if udpOwner != "B" {
		t.Errorf("configuredSeedOwnerUDP[%s] = %q, want %q (a plain loopback UDP handshake)", seed, udpOwner, "B")
	}
	if tcpOwner != "" {
		t.Errorf("configuredSeedOwnerTCP[%s] = %q, want empty -- this connection never went over a TCP fallback", seed, tcpOwner)
	}
}

// A node once confirmed (via a completed handshake -- see
// handshake_engine.go's onHSResp) at a configured seed address still reads
// as a seed even after its live session has since moved to a completely
// different address, as long as configuredSeedOwnerUDP/TCP still attribute
// that seed address to it. This is the real-fleet scenario that motivated
// adding this: a seed's live path had moved to a private LAN shortcut, no
// longer matching its public configured address at all.
func TestManagedPeersIsSeedSurvivesRoamingViaSeedOwner(t *testing.T) {
	seedAddr := netip.MustParseAddrPort("203.0.113.9:65432")
	roamedAddr := netip.MustParseAddrPort("192.168.1.50:41000") // a private LAN shortcut, nothing like the seed

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{seedAddr}
	ns.configuredSeedOwnerUDP[seedAddr] = "a" // "a" was, at some point, confirmed reachable at the seed address over UDP

	addManagedNode(ns, "a", roamedAddr) // but is live at a totally different address now

	peers := e.ManagedPeers(0)
	if len(peers) != 1 || !peers[0].IsSeed {
		t.Errorf("roamed node with a configuredSeedOwnerUDP attribution: IsSeed = %v, want true", len(peers) == 1 && peers[0].IsSeed)
	}
}

// Two distinct seeds configured at the identical host:port, disambiguated
// only by UDP vs TCP -- a real pattern on the fleet this shipped for: two
// peers behind one NAT gateway, port-forwarded to the same external
// host:port on different protocols. An earlier version of this attribution
// (keyed by address alone, via the shared ns.seedOwner) could only ever
// credit that one shared key to whichever seed most recently completed a
// handshake, leaving the other permanently unrecognizable. Splitting
// attribution by transport (configuredSeedOwnerUDP vs configuredSeedOwnerTCP)
// resolves it: this test proves both "a" (tcp) and "b" (udp) are
// independently recognized, each roamed to its own unrelated address, with
// nothing but transport telling them apart at the shared key.
func TestManagedPeersIsSeedResolvesTwoSeedsSharingOneAddressByTransport(t *testing.T) {
	sharedSeedAddr := netip.MustParseAddrPort("174.64.247.165:65432") // configured for both "a" (tcp) and "b" (udp)

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredTCPSeeds = []netip.AddrPort{sharedSeedAddr} // "a"'s seed entry
	ns.configuredSeeds = []netip.AddrPort{sharedSeedAddr}    // "b"'s seed entry -- same address, disambiguated only by which list it's in
	ns.configuredSeedOwnerTCP[sharedSeedAddr] = "a"          // "a" confirmed over the TCP fallback
	ns.configuredSeedOwnerUDP[sharedSeedAddr] = "b"          // "b" confirmed over plain UDP -- a separate map, no collision

	addManagedNode(ns, "a", netip.MustParseAddrPort("192.168.1.50:41000")) // roamed; recognized via configuredSeedOwnerTCP
	addManagedNode(ns, "b", netip.MustParseAddrPort("198.51.100.9:33000")) // also roamed; recognized via configuredSeedOwnerUDP

	peers := e.ManagedPeers(0)
	if len(peers) != 2 {
		t.Fatalf("got %d managed peers, want 2", len(peers))
	}
	got := map[string]bool{}
	for _, p := range peers {
		got[p.NodeID] = p.IsSeed
	}
	if !got["a"] {
		t.Errorf("a (holds the TCP half of the shared address): IsSeed = false, want true")
	}
	if !got["b"] {
		t.Errorf("b (holds the UDP half of the shared address): IsSeed = false, want true -- " +
			"if this fails, transport-based attribution has regressed back to the address-only ambiguity")
	}
}

func TestManagedPeersIsSeedClassification(t *testing.T) {
	seedAddr := netip.MustParseAddrPort("203.0.113.9:65432")
	notSeedAddr := netip.MustParseAddrPort("198.51.100.4:65432")

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{seedAddr}

	// "a" sits at the configured seed's exact address:port. "b" is a
	// perfectly ordinary peer at some other address entirely.
	addManagedNode(ns, "a", seedAddr)
	addManagedNode(ns, "b", notSeedAddr)

	peers := e.ManagedPeers(0)
	if len(peers) != 2 {
		t.Fatalf("got %d managed peers, want 2", len(peers))
	}
	got := map[string]bool{}
	for _, p := range peers {
		got[p.NodeID] = p.IsSeed
	}
	if !got["a"] {
		t.Errorf("node at the seed's exact address: IsSeed = false, want true")
	}
	if got["b"] {
		t.Errorf("ordinary node: IsSeed = true, want false")
	}
}

// Regression test #1, for the first real bug found: several peers on one
// operator's actual fleet shared a single public IP behind one NAT gateway/
// office router, differing only by port -- the same shape as gn-debian,
// gn-freebsd, gn-macos, gn-manjaro, gn-rocky, gn-win10, and gn-win11 all
// reporting 174.64.247.165 with different ports in a real tshoot bundle. An
// earlier version of this check compared IP only (deliberately ignoring
// port, on the theory that a seed's observed port could drift across
// transport paths), which meant EVERY peer sharing that IP got flagged as a
// seed instead of just the one actually configured as one.
func TestManagedPeersIsSeedDoesNotMatchOtherPeerSharingSeedIP(t *testing.T) {
	sharedIP := netip.MustParseAddr("174.64.247.165")
	seedAddr := netip.AddrPortFrom(sharedIP, 65432) // the actual configured seed

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{seedAddr}

	addManagedNode(ns, "the-real-seed", seedAddr)
	addManagedNode(ns, "sibling-behind-same-nat-1", netip.AddrPortFrom(sharedIP, 41104))
	addManagedNode(ns, "sibling-behind-same-nat-2", netip.AddrPortFrom(sharedIP, 63064))

	peers := e.ManagedPeers(0)
	if len(peers) != 3 {
		t.Fatalf("got %d managed peers, want 3", len(peers))
	}
	for _, p := range peers {
		want := p.NodeID == "the-real-seed"
		if p.IsSeed != want {
			t.Errorf("%s: IsSeed = %v, want %v", p.NodeID, p.IsSeed, want)
		}
	}
}

// Regression test #2, for a second, independent real bug found on the same
// fleet immediately after the port-exactness fix above shipped: ManagedPeers
// read ns.seeds/ns.tcpSeeds directly, which are NOT a clean view of the
// operator's configured Seeds -- cmd/gravinet builds them from Seeds AND the
// entire PeerCache (every address any peer has ever been seen at, kept
// around purely to speed up reconnecting after a restart; see
// NetSpec.ConfiguredSeeds' doc comment). On the real fleet this meant a
// perfectly ordinary peer -- one never configured as a seed at all --  still
// got flagged as one, simply because its current address happened to appear
// in PeerCache (which is nearly guaranteed for any peer that's ever
// reconnected). Exercised here by putting an address in ns.seeds (the
// contaminated operational list) that is deliberately absent from
// ns.configuredSeeds (the clean one) -- ManagedPeers must consult the
// latter, not the former.
func TestManagedPeersIsSeedIgnoresPeerCacheContamination(t *testing.T) {
	realSeed := netip.MustParseAddrPort("203.0.113.9:65432")
	peerCacheOnly := netip.MustParseAddrPort("198.51.100.4:52341") // never configured as a seed

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{realSeed}      // the clean, operator-authored list
	ns.seeds = []netip.AddrPort{realSeed, peerCacheOnly} // the contaminated live dial set, as cmd/gravinet actually builds it

	addManagedNode(ns, "actual-seed", realSeed)
	addManagedNode(ns, "ordinary-peer-in-peer-cache", peerCacheOnly)

	peers := e.ManagedPeers(0)
	if len(peers) != 2 {
		t.Fatalf("got %d managed peers, want 2", len(peers))
	}
	got := map[string]bool{}
	for _, p := range peers {
		got[p.NodeID] = p.IsSeed
	}
	if !got["actual-seed"] {
		t.Errorf("actual-seed: IsSeed = false, want true")
	}
	if got["ordinary-peer-in-peer-cache"] {
		t.Errorf("ordinary-peer-in-peer-cache: IsSeed = true, want false -- matched via ns.seeds' PeerCache contamination, not ns.configuredSeeds")
	}
}

// A configuredTCPSeeds entry counts exactly the same as a configuredSeeds
// one -- an operator dialing a network's TCP/TLS-fallback seed directly is
// still reaching one of its bootstrap points, not just any peer.
func TestManagedPeersIsSeedMatchesTCPSeedsToo(t *testing.T) {
	tcpSeed := netip.MustParseAddrPort("203.0.113.9:65432")

	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredTCPSeeds = []netip.AddrPort{tcpSeed}
	addManagedNode(ns, "a", tcpSeed)

	peers := e.ManagedPeers(0)
	if len(peers) != 1 || !peers[0].IsSeed {
		t.Errorf("node at a configured tcpSeeds address: IsSeed = %v, want true", len(peers) == 1 && peers[0].IsSeed)
	}
}

// A node that predates endpoint tracking (or was only ever learned via
// gossip, never a direct handshake) has no endpoint to compare at all --
// that must read as "not a seed", never a panic or a false positive from a
// zero-value AddrPort accidentally matching something.
func TestManagedPeersIsSeedFalseWithNoEndpoint(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ns.configuredSeeds = []netip.AddrPort{netip.MustParseAddrPort("203.0.113.9:65432")}
	addManagedNode(ns, "a", netip.AddrPort{}) // zero value: no endpoint known

	peers := e.ManagedPeers(0)
	if len(peers) != 1 || peers[0].IsSeed {
		t.Errorf("node with no known endpoint: IsSeed = %v, want false", len(peers) == 1 && peers[0].IsSeed)
	}
}
