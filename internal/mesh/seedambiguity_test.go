package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// The gn-ionos2 / mcfed failure, reproduced from the operator's actual config.
//
// mcfed's seed list named gn-ionos2 twice, once per address family, both with a
// tcp:// scheme:
//
//	tcp://66.179.240.44:65432,23,70,...            "ionos gn-ionos2 vps los angeles"
//	tcp://[2607:f1c0:f00c:db01::1]:65432,23,70,... "ionos gn-ionos2 vps los angeles"
//
// gn-ionos2 also answers UDP on 65432 (udp_ports and tcp_ports are both
// [65432] — the default shape), so those host:ports were learned as UDP seeds
// too, and cmd/gravinet folds peer_cache into the UDP seed list, which put
// 66.179.240.44:65432 there a second way. explicitSeed is a union over both
// lists, so the address satisfied "explicitly configured on both transports"
// without any operator having configured two peers.
//
// The old ambiguity test stopped there and returned true. Owner became
// ownerAmbiguous, ConflictsWith matched every candidate for gn-ionos2, and
// mcfed skipped the direct dial on every tick from 16:31 onward — 105 times in
// the captured log, with no event that could ever clear it. It had been
// dialing that same address successfully for hours beforehand.
//
// One node on two transports at one address is not ambiguous. It must dial.
func TestOnePeerOnBothTransportsIsNotAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
	}{
		{"ipv4", "66.179.240.44:65432"},
		{"ipv6", "[2607:f1c0:f00c:db01::1]:65432"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ns := testEngineWithNet(t)
			e.SetFallbackPort(65432)
			netID := ns.spec.ID
			ep := netip.MustParseAddrPort(tc.addr)

			// The tcp:// config entry.
			ns.mu.Lock()
			ns.tcpSeeds = append(ns.tcpSeeds, ep)
			ns.mu.Unlock()
			e.AddExplicitSeed(netID, ep)
			// The same host:port learned as a UDP seed once gn-ionos2 actually
			// connects over UDP — one node, both transports.
			e.AddSeedForProto(netID, ep, "gn-ionos2", ProtoUDP)

			ns.mu.RLock()
			ambiguous := ns.ambiguousSeedAddrLocked(ep)
			ns.mu.RUnlock()
			if ambiguous {
				t.Fatal("one node reachable on both transports at one address was called ambiguous; " +
					"that verdict permanently suppressed every direct path to it")
			}

			seeds := ns.seedCandidates()
			for _, s := range seeds {
				if s.Owner == ownerAmbiguous {
					t.Fatalf("seed %v carries the unattributable owner; ConflictsWith will block every candidate for its real owner", s)
				}
			}

			owner := ns.seedOwnerOf(ep)
			if owner != "gn-ionos2" {
				t.Fatalf("owner = %q, want gn-ionos2", owner)
			}
			dialable := false
			for _, c := range e.fallbackCandidates(ns, ep, 65432, owner) {
				if c.Proto == ProtoTCP && c.Port == 65432 && !c.ConflictsWith(seeds) {
					dialable = true
				}
			}
			if !dialable {
				t.Fatal("gn-ionos2's own configured seed was disqualified as a conflict with itself — " +
					"this is the bug that pinned mcfed to a relay path indefinitely")
			}
		})
	}
}

// The narrowed test must still catch what it was written for. cush1 on TCP and
// cush2 on UDP at one NAT address really are two peers, and a candidate derived
// from one protocol's seed must not be dialed at the other's listener.
func TestTwoDistinctOwnersRemainAmbiguous(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.SetFallbackPort(65432)
	netID := ns.spec.ID
	ep := netip.MustParseAddrPort("174.64.247.165:65432")

	ns.mu.Lock()
	ns.tcpSeeds = append(ns.tcpSeeds, ep)
	ns.mu.Unlock()
	e.AddExplicitSeed(netID, ep)
	e.AddSeedForProto(netID, ep, "cush1", ProtoTCP)
	e.AddSeedForProto(netID, ep, "cush2", ProtoUDP)

	ns.mu.RLock()
	n := ns.distinctSeedOwnersLocked(ep)
	ns.mu.RUnlock()
	if n < 2 {
		t.Fatalf("distinct owners = %d, want >= 2 — two peers share this host:port", n)
	}

	// And the end-to-end guard still refuses the cross-dial.
	seeds := ns.seedCandidates()
	owner := ns.seedOwnerOfProto(ep, ProtoUDP)
	for _, c := range e.fallbackCandidates(ns, ep, 65432, owner) {
		if c.Proto == ProtoTCP && c.Port == 65432 && !c.ConflictsWith(seeds) {
			t.Fatalf("would dial %v for %q — that is cush1's listener", c, c.Owner)
		}
	}
}

// A disagreement recorded only in the configuredSeedOwner* maps (populated by
// onHSResp for addresses that are genuinely in the configured seed lists) is
// still a disagreement, and must still register.
func TestConfiguredSeedOwnerDisagreementCounts(t *testing.T) {
	_, ns := testEngineWithNet(t)
	ep := netip.MustParseAddrPort("203.0.113.9:65432")

	ns.mu.Lock()
	ns.seedOwner[ep] = "peerA"
	ns.configuredSeedOwnerTCP[ep] = "peerB"
	n := ns.distinctSeedOwnersLocked(ep)
	ns.mu.Unlock()

	if n < 2 {
		t.Fatalf("distinct owners = %d, want >= 2 — the two attribution maps name different nodes", n)
	}
}

// Even a correct-looking conflict verdict must not be permanent. Nothing about
// being suppressed produces the evidence that would lift the suppression: the
// dial never happens, so no handshake completes, so no attribution is
// corrected. Once the owner has gone without a direct session for
// conflictSkipEscape, one probe goes out.
func TestConflictSkipEventuallyProbes(t *testing.T) {
	e, ns := testEngineWithNet(t)
	c := Candidate{
		Addr:  netip.MustParseAddr("198.51.100.20"),
		Port:  65432,
		Proto: ProtoTCP,
		Src:   SrcSeed,
		Owner: "peerA",
	}

	// First encounter starts the clock; it must not fire immediately, or the
	// guard would be worthless.
	if e.conflictSkipAllowed(ns, c, false) {
		t.Fatal("escaped on the very first suppression — the guard has to be allowed to be confident")
	}
	if e.conflictSkipAllowed(ns, c, false) {
		t.Fatal("escaped well inside the window")
	}

	// Age the clock past the window.
	ns.mu.Lock()
	ns.conflictSkipSince[c.Key()] = time.Now().Add(-conflictSkipEscape - time.Minute)
	ns.mu.Unlock()

	if !e.conflictSkipAllowed(ns, c, false) {
		t.Fatalf("still suppressed after %s with no session — this is the state that has no exit", conflictSkipEscape)
	}
	// And it paces itself rather than dialing every tick from then on.
	if e.conflictSkipAllowed(ns, c, false) {
		t.Fatal("probed twice in one window; the escape must be paced")
	}
}

// While the owner is directly connected the guard costs nothing, so it must
// never accrue toward an escape.
func TestConflictSkipDoesNotAccrueWhileConnected(t *testing.T) {
	e, ns := testEngineWithNet(t)
	c := Candidate{
		Addr:  netip.MustParseAddr("198.51.100.21"),
		Port:  65432,
		Proto: ProtoTCP,
		Src:   SrcSeed,
		Owner: "peerA",
	}

	e.conflictSkipAllowed(ns, c, false)
	ns.mu.Lock()
	ns.conflictSkipSince[c.Key()] = time.Now().Add(-conflictSkipEscape - time.Minute)
	ns.mu.Unlock()

	if e.conflictSkipAllowed(ns, c, true) {
		t.Fatal("escaped while the owner had a direct session; there is nothing to recover from")
	}
	ns.mu.RLock()
	_, still := ns.conflictSkipSince[c.Key()]
	ns.mu.RUnlock()
	if still {
		t.Fatal("clock survived a period of connectivity; suppression time must be continuous to count")
	}
}

// An operator maintaining one seed list across a fleet lists every node in it,
// including the node the file is on. gn-ionos2's own config named
// tcp://66.179.240.44:65432 — its own address — so it dialed itself on every
// tick and dropped its own handshake as "claims our own node id" 3,201 times in
// the captured log.
func TestOwnAddressIsNotAcceptedAsSeed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	netID := ns.spec.ID

	self := netip.MustParseAddr("66.179.240.44")
	own := map[netip.Addr]bool{self: true}
	e.ownAddrs.Store(&own)

	ep := netip.AddrPortFrom(self, 65432)
	e.AddExplicitSeed(netID, ep)
	e.addTCPSeed(netID, ep)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, s := range ns.seeds {
		if s == ep {
			t.Fatal("this host's own address was registered as a UDP seed; every dial to it reaches this daemon")
		}
	}
	for _, s := range ns.tcpSeeds {
		if s == ep {
			t.Fatal("this host's own address was registered as a TCP seed; primeTCPSeeds will dial it every tick")
		}
	}
}

// The self-address rejection must not reach loopback. Several distinct nodes
// legitimately share 127.0.0.1 and differ only by port — that is how most of
// this package's own tests are built, and how a multi-node host deploys.
func TestLoopbackSeedsSurviveOwnAddressCheck(t *testing.T) {
	e, ns := testEngineWithNet(t)
	netID := ns.spec.ID

	lo := netip.MustParseAddr("127.0.0.1")
	own := map[netip.Addr]bool{lo: true}
	e.ownAddrs.Store(&own)

	ep := netip.AddrPortFrom(lo, 45999)
	e.AddExplicitSeed(netID, ep)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, s := range ns.seeds {
		if s == ep {
			return
		}
	}
	t.Fatal("a loopback seed was rejected as this host's own address; peers on one host share it by design")
}
