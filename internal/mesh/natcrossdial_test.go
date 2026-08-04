package mesh

import (
	"net/netip"
	"testing"
)

// The failure this whole line of work started from, reproduced from the
// operator's actual seed list and driven through the real registration path.
//
//	tcp://174.64.247.165:65432   "cox gn-cush1 nat phoenix"
//	     174.64.247.165:65432    "cox-gn-cush2 nat phoenix"
//
// Two peers behind one NAT gateway, port-forwarded to the same external
// address on different protocols. TCP/65432 and UDP/65432 are independent
// mappings reaching different hosts, and the operator's own notes said which
// was which — but the manager node dialed cush2 over TCP at that address and
// landed on cush1's listener. Deterministically, every tick: tx=28, rx=0, and
// a session that flapped up-and-pruned every fifteen seconds for hours.
//
// Three things had to be true for it to happen, and each was fixed separately:
// the engine derived a peer's TCP port by walking sessions for one that shared
// an IP (v788), the config shape modelled TCP as a tier derived from UDP
// (v789/v790/v791), and — the part that kept the failure alive after all of
// that — seed ownership was keyed by address alone, so cush1's TCP seed
// reported as belonging to cush2 and the guard meant to catch this compared a
// candidate against its own owner and passed.
//
// This test is the end-to-end statement of the whole thing: given that seed
// list, the derived TCP candidate for cush2 must not be dialed.
func TestTwoPeersOneNATDoNotCrossDial(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.SetFallbackPort(65432)
	netID := ns.spec.ID
	ep := netip.MustParseAddrPort("174.64.247.165:65432")

	// Register both seeds the way config load does: the tcp:// one lands in
	// tcpSeeds, both are explicit, and each carries its owner.
	ns.mu.Lock()
	ns.tcpSeeds = append(ns.tcpSeeds, ep)
	ns.mu.Unlock()
	e.AddExplicitSeed(netID, ep)
	e.AddSeedForProto(netID, ep, "cush1", ProtoTCP) // the tcp:// seed
	e.AddSeedForProto(netID, ep, "cush2", ProtoUDP) // the UDP seed, identical host:port

	seeds := ns.seedCandidates()
	if len(seeds) == 0 {
		t.Fatal("no configured seeds in the conflict set — nothing would ever be checked against, which is how the guard silently passed before")
	}

	// What ensureFallback builds when it tries to reach cush2 over TCP.
	owner := ns.seedOwnerOfProto(ep, ProtoUDP)
	for _, c := range e.fallbackCandidates(ns, ep, 65432, owner) {
		if c.Proto == ProtoTCP && c.Port == 65432 && !c.ConflictsWith(seeds) {
			t.Fatalf("would dial %v for %q — that is cush1's configured TCP listener, and this is the original failure", c, c.Owner)
		}
	}
}

// The guard must not become a blanket refusal. One peer behind a NAT is the
// ordinary case and its own seed must stay dialable — a candidate that cannot
// be dialed is worse than one dialed at a wrong port, because no answer means
// no connectivity.
func TestSinglePeerBehindNATStillDialable(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.SetFallbackPort(65432)
	netID := ns.spec.ID
	ep := netip.MustParseAddrPort("198.51.100.7:65432")

	ns.mu.Lock()
	ns.tcpSeeds = append(ns.tcpSeeds, ep)
	ns.mu.Unlock()
	e.AddExplicitSeed(netID, ep)
	e.AddSeedForProto(netID, ep, "peerA", ProtoTCP)

	seeds := ns.seedCandidates()
	owner := ns.seedOwnerOfProto(ep, ProtoTCP)
	if owner != "peerA" {
		t.Fatalf("owner = %q, want peerA — one peer at an address is unambiguous", owner)
	}
	dialable := false
	for _, c := range e.fallbackCandidates(ns, ep, 65432, owner) {
		if c.Proto == ProtoTCP && c.Port == 65432 && !c.ConflictsWith(seeds) {
			dialable = true
		}
	}
	if !dialable {
		t.Fatal("a peer's own configured seed was disqualified; the guard must only fire across owners")
	}
}

// seedOwnerProto is what makes the attribution possible. Keyed by address
// alone, both seeds share one entry and whichever registered last owns both —
// which is precisely why ConflictsWith could not see a cross-owner dial.
func TestSeedOwnerIsPerProtocol(t *testing.T) {
	e, ns := testEngineWithNet(t)
	netID := ns.spec.ID
	ep := netip.MustParseAddrPort("174.64.247.165:65432")

	ns.mu.Lock()
	ns.tcpSeeds = append(ns.tcpSeeds, ep)
	ns.mu.Unlock()
	e.AddSeedForProto(netID, ep, "cush1", ProtoTCP)

	if got := ns.seedOwnerOfProto(ep, ProtoTCP); got != "cush1" {
		t.Errorf("tcp owner = %q, want cush1", got)
	}
	ns.mu.RLock()
	shared := ns.seedOwner[ep]
	ns.mu.RUnlock()
	if shared == "" {
		t.Error("the address-only map should still be populated for everything that reads it")
	}
}
