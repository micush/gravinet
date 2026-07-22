package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestUsableLocalCandidateFilters: only addresses a peer could actually dial
// get advertised. Loopback is the dangerous one — a peer that added our
// 127.0.0.1 as a seed would dial *its own* loopback, and on a shared host (or
// in these tests, where every node is on 127.0.0.1) reach an entirely
// different node. Link-local needs a zone this encoding doesn't carry, so a
// peer dialing it would reach whatever holds that address on *its* link.
func TestUsableLocalCandidateFilters(t *testing.T) {
	bad := []string{
		"127.0.0.1",   // loopback v4
		"::1",         // loopback v6
		"169.254.5.5", // link-local v4
		"fe80::1",     // link-local v6
		"224.0.0.1",   // multicast
		"0.0.0.0",     // unspecified
	}
	for _, s := range bad {
		if usableLocalCandidate(netip.MustParseAddr(s)) {
			t.Errorf("%s must not be advertised as a host candidate", s)
		}
	}
	// Private LAN ranges are the entire point of the feature — they're what
	// makes two nodes behind one NAT able to find each other. Public addresses
	// are simply correct candidates for an un-NATed host.
	good := []string{"192.168.55.33", "10.0.0.4", "172.16.9.9", "100.64.0.1", "203.0.113.7", "2001:db8::1"}
	for _, s := range good {
		if !usableLocalCandidate(netip.MustParseAddr(s)) {
			t.Errorf("%s should be advertised as a host candidate", s)
		}
	}
}

// TestLocalEndpointsFallBackToTCPPortWhenUDPDisabled: with UDP off (PrimaryPort
// 0 — the '-' setting) the interface addresses are still perfectly dialable
// over the TCP/TLS fallback, so they must still be advertised, at the fallback
// port. Suppressing them would leave a UDP-disabled node with no LAN path to
// its same-NAT neighbours and nothing but a relay — which is exactly backwards,
// since a relay is its *only* alternative. Only when UDP and TCP/TLS are both
// off is there genuinely nothing dialable to offer.
func TestLocalEndpointsFallBackToTCPPortWhenUDPDisabled(t *testing.T) {
	// UDP on: candidates carry the UDP port.
	e := NewEngine(Options{NodeID: "self", PrimaryPort: 51820, TCPFallbackPort: 443})
	got := e.localEndpoints()
	if len(got) == 0 {
		t.Skip("no usable interface addresses on this host")
	}
	for _, ep := range got {
		if ep.Port() != 51820 {
			t.Errorf("UDP enabled: candidate %s should carry the primary UDP port 51820", ep)
		}
	}

	// UDP off, TCP/TLS on: still advertised, now at the fallback port.
	e.SetPrimaryPort(0)
	got = e.localEndpoints()
	if len(got) == 0 {
		t.Fatal("UDP disabled but TCP/TLS fallback enabled: host candidates must still be advertised — they are dialable over TCP, and a relay is this node's only alternative")
	}
	for _, ep := range got {
		if ep.Port() != 443 {
			t.Errorf("UDP disabled: candidate %s should carry the TCP/TLS fallback port 443", ep)
		}
		if !usableLocalCandidate(ep.Addr()) {
			t.Errorf("candidate %s should have been filtered out", ep)
		}
	}

	// Both off: nothing dialable, so nothing to advertise.
	e.SetFallbackPort(0)
	if got := e.localEndpoints(); len(got) != 0 {
		t.Fatalf("UDP and TCP/TLS both disabled: want no candidates, got %v", got)
	}
}

// TestTCPPortForHostCandidateUsesOwnersAdvertisedPort pins the other half of
// making host candidates work over TCP. A peer's LAN address is by definition
// not its observed endpoint, so tcpPortForEndpoint's exact-match on ni.endpoint
// can never hit for a candidate — before the owner lookup, every LAN candidate
// silently fell through to *our own* fallback port, which is correct only by
// coincidence (when both nodes happen to use the same one). Here the peer
// advertises 8443 while we use 443; dialing our own 443 at its LAN address
// would just fail.
func TestTCPPortForHostCandidateUsesOwnersAdvertisedPort(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.SetFallbackPort(443) // ours, deliberately different from the peer's
	lan := netip.MustParseAddrPort("192.168.55.1:65432")

	ns.mu.Lock()
	ns.seedOwner[lan] = "gn-cush1"                                      // as addLocalCandidates/AddSeedFor records
	ns.nodes["gn-cush1"] = &nodeInfo{nodeID: "gn-cush1", tcpPort: 8443, // what the peer actually advertised
		endpoint: netip.MustParseAddrPort("203.0.113.1:40001")} // its observed (shared public) endpoint
	ns.mu.Unlock()

	if got := ns.tcpPortForEndpoint(lan); got != 8443 {
		t.Fatalf("host candidate should resolve to its owner's advertised TCP port 8443, got %d", got)
	}
}

// TestLocalEndpointsSafeUnderNetLock pins the invariant that broke v377 in
// production: localEndpoints is called from buildHSInit, which planHandshake
// invokes *while holding ns.mu*, for every handshake packet this node builds.
// It therefore must be a pure atomic read — no syscall, no further locking.
// It used to enumerate interfaces inline (net.InterfaceAddrs, plus e.mu via
// isOverlayAddr) right there under the network's write lock. That was
// survivable only while host candidates were being pruned as fast as they were
// learned; once they persisted, ns.seeds grew to dozens of entries, initLoop
// began issuing a syscall per seed per tick with ns.mu held, and everything
// that needed that lock for more than an instant — the web admin above all —
// was starved out. The node still answered pings and could not be managed at
// all.
//
// If localEndpoints ever reacquires ns.mu (directly or through a helper), this
// deadlocks outright rather than merely being slow, which is the failure we
// want: loud, immediate, and in CI rather than on someone's mesh.
func TestLocalEndpointsSafeUnderNetLock(t *testing.T) {
	e, ns := testEngineWithNet(t)

	done := make(chan []netip.AddrPort, 1)
	go func() {
		ns.mu.Lock() // exactly as planHandshake holds it around buildHSInit
		defer ns.mu.Unlock()
		done <- e.localEndpoints()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("localEndpoints blocked while ns.mu was held — it must be a pure atomic read; the enumeration belongs in refreshLocalCandidates, off the handshake path")
	}
}

// TestRefreshLocalCandidatesPublishes: the refresh is what actually enumerates,
// and localEndpoints must see its result.
func TestRefreshLocalCandidatesPublishes(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", PrimaryPort: 51820, TCPFallbackPort: 443})
	// NewEngine primes the cache, so this is already populated without anyone
	// having called refresh explicitly — a handshake built before the first
	// maintenance tick still advertises candidates.
	primed := e.localEndpoints()
	if len(primed) == 0 {
		t.Skip("no usable interface addresses on this host")
	}
	for _, ep := range primed {
		if ep.Port() != 51820 {
			t.Fatalf("primed candidate %s should carry the primary UDP port", ep)
		}
	}
	// A port change must be reflected without waiting for a maintenance tick.
	e.SetPrimaryPort(0) // UDP off → fall back to the TCP/TLS port
	for _, ep := range e.localEndpoints() {
		if ep.Port() != 443 {
			t.Fatalf("after disabling UDP, candidate %s should carry the fallback port 443", ep)
		}
	}
}

// handshake encoding, and — critically — a payload from a peer that predates
// the field still decodes cleanly, leaving LocalEndpoints nil rather than
// erroring. The trailing-field nesting is what guarantees that; this pins it.
func TestHSPayloadRoundTripsLocalEndpoints(t *testing.T) {
	eps := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.55.33:65432"),
		netip.MustParseAddrPort("[2001:db8::1]:65432"),
	}
	in := hsPayload{Ephemeral: make([]byte, ephemeralLen), NodeID: "n", LocalEndpoints: eps}
	out, err := decodeHSPayload(encodeHSPayload(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.LocalEndpoints) != len(eps) {
		t.Fatalf("LocalEndpoints = %v, want %v", out.LocalEndpoints, eps)
	}
	for i := range eps {
		if out.LocalEndpoints[i] != eps[i] {
			t.Errorf("candidate %d = %s, want %s", i, out.LocalEndpoints[i], eps[i])
		}
	}

	// An older peer simply doesn't emit the field. Decoding must succeed with
	// no candidates, not fail — otherwise every handshake from a
	// not-yet-upgraded node would be rejected outright.
	noneIn := hsPayload{Ephemeral: make([]byte, ephemeralLen), NodeID: "n"}
	noneOut, err := decodeHSPayload(encodeHSPayload(noneIn))
	if err != nil {
		t.Fatalf("decode (no candidates): %v", err)
	}
	if len(noneOut.LocalEndpoints) != 0 {
		t.Fatalf("want no candidates, got %v", noneOut.LocalEndpoints)
	}
}

// TestPeerListRoundTripsLocalEndpoints covers the leg that actually fixes the
// same-NAT case. Two nodes behind one gateway never observe each other at all,
// so neither can learn the other's LAN address first-hand — it can only arrive
// via a mutual peer's gossip. That means the peer list, not just the
// handshake, has to carry it.
func TestPeerListRoundTripsLocalEndpoints(t *testing.T) {
	entries := []peerEntry{
		{
			nodeID:         "gn-cush1",
			hostname:       "gn-cush1",
			endpoint:       netip.MustParseAddrPort("203.0.113.1:40001"), // the shared public address
			localEndpoints: []netip.AddrPort{netip.MustParseAddrPort("192.168.55.1:65432")},
		},
		{
			nodeID:   "gn-freebsd",
			hostname: "gn-freebsd",
			endpoint: netip.MustParseAddrPort("198.51.100.9:65432"),
		},
	}
	got, err := decodePeerList(encodePeerList(entries))
	if err != nil {
		t.Fatalf("decodePeerList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if len(got[0].localEndpoints) != 1 || got[0].localEndpoints[0] != netip.MustParseAddrPort("192.168.55.1:65432") {
		t.Fatalf("cush1 host candidate lost in gossip: %v", got[0].localEndpoints)
	}
	if len(got[1].localEndpoints) != 0 {
		t.Fatalf("a peer with no candidates should decode to none, got %v", got[1].localEndpoints)
	}
	// A peer list from a mesh where nobody advertises candidates must not grow
	// a block at all, and must still decode.
	plain := []peerEntry{{nodeID: "x", endpoint: netip.MustParseAddrPort("203.0.113.5:1")}}
	back, err := decodePeerList(encodePeerList(plain))
	if err != nil || len(back) != 1 || len(back[0].localEndpoints) != 0 {
		t.Fatalf("plain peer list round trip failed: %v %v", back, err)
	}
}

// TestAddLocalCandidatesSeedsThePeersLANAddress is the end of the chain: a
// gossiped host candidate has to become an ordinary seed, attributed to its
// owner, so initSeedTick dials it like anything else. This is what turns
// "cush2 has heard that cush1 is at 192.168.55.1" into an actual direct
// handshake attempt instead of a permanent relay.
func TestAddLocalCandidatesSeedsThePeersLANAddress(t *testing.T) {
	e, ns := testEngineWithNet(t)
	lan := netip.MustParseAddrPort("192.168.55.1:65432")

	e.addLocalCandidates(ns.spec.ID, "gn-cush1", []netip.AddrPort{
		lan,
		netip.MustParseAddrPort("127.0.0.1:65432"), // must be ignored even if a peer claims it
	})

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	var found bool
	for _, s := range ns.seeds {
		if s == lan {
			found = true
		}
		if s.Addr().IsLoopback() {
			t.Errorf("a peer's loopback claim must never become a seed; seeds=%v", ns.seeds)
		}
	}
	if !found {
		t.Fatalf("host candidate %s should have been seeded; seeds=%v", lan, ns.seeds)
	}
	if ns.seedOwner[lan] != "gn-cush1" {
		t.Errorf("host candidate should be attributed to its node so install() can prune it later; got %q", ns.seedOwner[lan])
	}
}

// TestAddLocalCandidatesIgnoresSelf: our own candidates coming back to us via
// gossip must never be seeded — install() already refuses to register a
// session for our own node id, but dialing ourselves would waste handshakes
// and pollute the seed list.
func TestAddLocalCandidatesIgnoresSelf(t *testing.T) {
	e, ns := testEngineWithNet(t)
	before := len(ns.seeds)
	e.addLocalCandidates(ns.spec.ID, e.nodeID, []netip.AddrPort{netip.MustParseAddrPort("192.168.55.9:65432")})
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if len(ns.seeds) != before {
		t.Fatalf("our own host candidates must not be seeded; seeds=%v", ns.seeds)
	}
}

// TestHostCandidateSweptOnShortGraceAndNotResurrected bounds what a host
// candidate is allowed to cost. Every candidate was being treated as a real
// seed: deadSeedGrace (an hour) of UDP handshake cycles, plus a TCP/TLS dial on
// every tick it cooled down. In a mesh of N peers each advertising several LAN
// addresses, most are unreachable from any given node by construction — so a
// node ground continuously through dozens of duds, which starved the web admin
// and delayed keepalives enough that healthy direct sessions timed out
// (peerTimeout 20s) and were replaced by relayed ones. A candidate is a
// same-link address: if it works, it works instantly. So it gets hostCandGrace,
// and once written off it must stay written off — gossip re-delivers every
// peer's full candidate list every interval, so without a dead-list the sweep
// and the re-add chase each other forever.
func TestHostCandidateSweptOnShortGraceAndNotResurrected(t *testing.T) {
	e, ns := testEngineWithNet(t)
	dud := netip.MustParseAddrPort("10.9.9.9:65432") // a peer's LAN address, unreachable from here

	e.addLocalCandidates(ns.spec.ID, "gn-ionos1", []netip.AddrPort{dud})
	ns.mu.Lock()
	present := false
	for _, s := range ns.seeds {
		if s == dud {
			present = true
		}
	}
	ns.seedFirstSeen[dud] = time.Now().Add(-2 * hostCandGrace) // past its short grace
	ns.mu.Unlock()
	if !present {
		t.Fatal("precondition: candidate should have been seeded")
	}

	// A real seed would still be well inside deadSeedGrace (an hour) here; a
	// host candidate must already be gone.
	e.sweepDeadSeeds(ns, time.Now())
	ns.mu.RLock()
	stillThere, markedDead := false, ns.hostCandDead[dud]
	for _, s := range ns.seeds {
		if s == dud {
			stillThere = true
		}
	}
	ns.mu.RUnlock()
	if stillThere {
		t.Fatalf("a host candidate past hostCandGrace that never connected must be swept; seeds=%v", ns.seeds)
	}
	if !markedDead {
		t.Fatal("a swept host candidate must be remembered as dead, or the next gossip re-adds it")
	}

	// Gossip re-delivers it, as it does every interval. It must not come back.
	e.addLocalCandidates(ns.spec.ID, "gn-ionos1", []netip.AddrPort{dud})
	ns.mu.RLock()
	resurrected := false
	for _, s := range ns.seeds {
		if s == dud {
			resurrected = true
		}
	}
	ns.mu.RUnlock()
	if resurrected {
		t.Fatalf("a written-off host candidate must not be resurrected by gossip; seeds=%v", ns.seeds)
	}

	// ...until our own attachment to the network changes, at which point every
	// "unreachable from here" verdict was made against an attachment that no
	// longer exists.
	e.clearDeadHostCands()
	e.addLocalCandidates(ns.spec.ID, "gn-ionos1", []netip.AddrPort{dud})
	ns.mu.RLock()
	retried := false
	for _, s := range ns.seeds {
		if s == dud {
			retried = true
		}
	}
	ns.mu.RUnlock()
	if !retried {
		t.Fatal("after our own candidate set changes, a previously-dead candidate deserves another chance")
	}
}

// TestHostCandidateOfUDPDisabledPeerIsDialedOverTCP is the regression that cost
// the most debugging in this whole sequence, and it was self-inflicted.
//
// v379, trying to stop a TCP dial storm, skipped the fallback dial for any host
// candidate whenever *this* node had UDP enabled. That keys the decision on the
// wrong end. Whether a TCP dial is worth making depends on whether the PEER can
// be reached over UDP, not on whether WE can speak it. A peer with UDP disabled
// (PrimaryPort 0 — the '-' setting) advertises its LAN address at its TCP/TLS
// port and can be reached over nothing else. Any peer that still had UDP enabled
// would probe that address over UDP, hear nothing (there is no UDP listener
// there), and then decline to try TCP — because its own UDP worked fine. Two
// machines on the same switch, one of them TCP-only, failed every direct attempt
// and relayed to each other across the internet, with the LAN path suppressed by
// the very code meant to be conserving effort. Suppression was the wrong tool;
// rate-limiting (fallbackDialCooldown) is.
func TestHostCandidateOfUDPDisabledPeerIsDialedOverTCP(t *testing.T) {
	e := NewEngine(Options{
		NodeID:          "self",
		PrimaryPort:     65432, // OUR udp works fine — this is what used to suppress the dial
		TCPFallbackPort: 443,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}},
	})
	f := &fakeFallback{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.network(1)

	// The peer has UDP disabled, so its host candidate is reachable only over
	// TCP/TLS. Nothing in the candidate itself says so — and nothing needs to.
	cand := netip.MustParseAddrPort("192.168.55.3:65432")
	e.addLocalCandidates(1, "gn-cush1", []netip.AddrPort{cand})
	e.ensureFallback(ns, cand)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.dials()) == 0 {
		t.Fatal("a host candidate must get a TCP/TLS dial even when this node's own UDP works — the peer may be TCP-only, and suppressing the dial leaves it reachable by nothing but a relay")
	}
}

// TestFallbackDialIsPacedNotSuppressed: the storm that v379 was right to worry
// about is bounded by a per-seed cooldown instead of by refusing to dial. The
// first attempt is always immediate; a retry waits out fallbackDialCooldown.
func TestFallbackDialIsPacedNotSuppressed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.168.55.3:65432")
	now := time.Now()

	if !e.fallbackDialDue(ns, seed, now) {
		t.Fatal("the first fallback dial for a seed must be immediate")
	}
	if e.fallbackDialDue(ns, seed, now.Add(time.Second)) {
		t.Fatal("a repeat dial to the same seed a tick later must be paced — that per-tick re-dial across dozens of seeds was the storm")
	}
	if !e.fallbackDialDue(ns, seed, now.Add(fallbackDialCooldown+time.Second)) {
		t.Fatal("after the cooldown elapses the seed must be retried — a peer that just became reachable over TCP has to be picked up")
	}
	// Pacing is per-seed: a different address is unaffected.
	other := netip.MustParseAddrPort("192.168.55.9:65432")
	if !e.fallbackDialDue(ns, other, now.Add(time.Second)) {
		t.Fatal("the cooldown must be per-seed, not global")
	}
}

// TestHostCandGraceOutlastsUpgradeThrottle guards the interaction that made
// v379's host candidates useless for the one case they exist to serve.
//
// The most valuable candidate is the one belonging to a peer we currently reach
// only via a relay — that is the whole point of the feature. But an upgrade
// attempt toward a relay-only peer is throttled to one per
// directUpgradeInterval (5 minutes) unless that peer is an explicit seed. With
// hostCandGrace at 90s, such a candidate got exactly ONE dial attempt before
// sweepDeadSeeds wrote it off and hostCandDead barred gossip from ever bringing
// it back: a single shot at its only job, then gone permanently. The grace must
// outlast the throttle by enough for several attempts.
func TestHostCandGraceOutlastsUpgradeThrottle(t *testing.T) {
	if hostCandGrace <= directUpgradeInterval {
		t.Fatalf("hostCandGrace (%v) must exceed directUpgradeInterval (%v), or a relay-only peer's candidate is swept before it can be retried even once", hostCandGrace, directUpgradeInterval)
	}
	if hostCandGrace < 3*directUpgradeInterval {
		t.Errorf("hostCandGrace (%v) leaves fewer than 3 throttled upgrade attempts (interval %v) before a candidate is permanently written off", hostCandGrace, directUpgradeInterval)
	}
	if hostCandGrace >= deadSeedGrace {
		t.Errorf("hostCandGrace (%v) should stay well below deadSeedGrace (%v) — a speculative same-link address does not deserve a real seed's patience", hostCandGrace, deadSeedGrace)
	}
}

// TestHostCandidateUpgradeIsNotThrottled: a host candidate belonging to a peer
// we reach only via relay is the highest-value dial in the system — it is the
// entire reason the feature exists — and it is a same-link UDP packet, about
// the cheapest thing the init loop can do. It must not be gated behind
// directUpgradeInterval (5 minutes), which is calibrated for speculative WAN
// retries against a peer a relay is already covering for.
//
// Before this, the escape from relay was throttled to one probe per five
// minutes unless the peer happened to be in explicitSeedNode — a map keyed by
// node ID and populated only when gossip attributes a node's *configured seed
// address* to it. So whether two machines on the same switch found each other
// promptly depended on whether an address in someone's config matched the
// endpoint gossip reported. It should not.
func TestHostCandidateUpgradeIsNotThrottled(t *testing.T) {
	e, ns := testEngineWithNet(t)
	lan := netip.MustParseAddrPort("192.168.55.3:65432")
	now := time.Now()

	// A peer reached only through a relay, NOT an explicit seed of ours.
	relay := &peerSession{nodeID: "gn-freebsd"}
	peer := &peerSession{nodeID: "gn-cush1", relay: relay}
	e.addLocalCandidates(ns.spec.ID, "gn-cush1", []netip.AddrPort{lan})
	ns.mu.Lock()
	ns.byNode["gn-cush1"] = peer
	if ns.explicitSeedNode["gn-cush1"] {
		t.Fatal("precondition: this peer must not be an explicit seed node")
	}
	ns.mu.Unlock()

	if !e.seedOwnerNeedsUpgrade(ns, lan, now) {
		t.Fatal("a host candidate for a relay-only peer should be due immediately")
	}
	// A moment later it must STILL be due — directUpgradeInterval must not have
	// been armed against it. (A plain gossip-learned endpoint would be throttled
	// here for the next 5 minutes; see TestSeedOwnerNeedsUpgradeThrottles.)
	if !e.seedOwnerNeedsUpgrade(ns, lan, now.Add(2*time.Second)) {
		t.Fatal("a host candidate must not be throttled by directUpgradeInterval — that is a 5-minute gate on the one probe that escapes the relay")
	}

	// It is still paced, though: once a handshake cycle exhausts and sets
	// seedBackoff, it waits out the cooldown like any other seed rather than
	// firing every tick forever.
	ns.mu.Lock()
	ns.seedBackoff[lan] = now.Add(seedRetryBackoff)
	ns.mu.Unlock()
	if e.seedOwnerNeedsUpgrade(ns, lan, now.Add(time.Second)) {
		t.Fatal("a host candidate cooling down in seedBackoff should wait, not retry every tick")
	}
	if !e.seedOwnerNeedsUpgrade(ns, lan, now.Add(seedRetryBackoff+time.Second)) {
		t.Fatal("after its cooldown elapses, the candidate should be retried")
	}
}

// TestHostCandidateDialedOverTCPWhenPeerHasUDPDisabled is the scenario that
// actually broke, end to end, and that none of the other tests here covered.
//
// gn-cush1 had UDP turned off ('-' in the port field, PrimaryPort 0). It is on
// the same switch as gn-cush2, which has UDP enabled. cush1 therefore advertises
// its LAN address at its TCP/TLS port (v375) and can be reached over nothing
// else. cush2 seeds that candidate, probes it over UDP, and hears silence —
// correctly, since there is no UDP listener there.
//
// v379's guard then skipped the TCP dial for host candidates whenever *this*
// node had UDP working. Which cush2 does. So the one path that could ever have
// worked was suppressed by the code meant to be conserving effort, and two
// machines on the same switch relayed to each other across the internet. The
// guard asked the wrong question: whether a TCP dial is worth making depends on
// whether the PEER can be reached over UDP, not on whether WE can speak it.
//
// The engine here has UDP enabled — that is the whole point of the test.
func TestHostCandidateDialedOverTCPWhenPeerHasUDPDisabled(t *testing.T) {
	e := NewEngine(Options{
		NodeID:          "gn-cush2",
		PrimaryPort:     65432, // OUR UDP works fine. Irrelevant to the peer.
		TCPFallbackPort: 443,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}},
	})
	f := &fakeFallback{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.network(1)

	// cush1's LAN address, advertised by a peer that cannot speak UDP at all.
	cand := netip.MustParseAddrPort("192.168.55.3:443")
	e.addLocalCandidates(1, "gn-cush1", []netip.AddrPort{cand})

	// The UDP probe has failed and the seed is cooling down — exactly the state
	// initSeedTick's backoff branch calls ensureFallback in.
	e.ensureFallback(ns, cand)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.dials()) == 0 {
		t.Fatal("a host candidate belonging to a UDP-disabled peer must be dialed over TCP/TLS — it is the only path to that peer that exists. Suppressing it because OUR OWN UDP happens to work is what made two machines on the same switch relay across the internet.")
	}
}

// TestVirtualBridgeIfaceExcludesVMPlumbingButNotRealBridges: addresses on
// host-local virtual networks are identical on every host running the same
// stack — libvirt puts 192.168.122.1 on virbr0 everywhere, Docker puts
// 172.17.0.1 on docker0 everywhere. Advertising one tells a peer "reach me
// here", and any peer running the same stack then dials its OWN bridge and
// talks to itself. Seen in production exactly so: mcfed advertised
// 192.168.122.1 and gn-cush1 logged "learned host candidate 192.168.122.1:65432
// for peer 0916a3a70b1d5f4c" — for an address cush1 also owns.
//
// The interesting half of this test is what must NOT be excluded. A bare "br0"
// is very often the *real* LAN bridge on a hypervisor host — the actual uplink,
// holding the address a peer genuinely should dial. Filtering it would break the
// same-LAN discovery this mechanism exists for. Docker's per-network bridges are
// "br-<hex>", which "br-" catches without touching br0.
func TestVirtualBridgeIfaceExcludesVMPlumbingButNotRealBridges(t *testing.T) {
	for _, name := range []string{"virbr0", "virbr1", "docker0", "br-1a2b3c4d5e6f", "veth9f2a", "vboxnet0", "vmnet8", "lxcbr0", "lxdbr0", "podman0", "cni0", "flannel.1"} {
		if !virtualBridgeIface(name) {
			t.Errorf("%s is host-local VM/container plumbing; its address is the same on every such host and must not be advertised", name)
		}
	}
	for _, name := range []string{"br0", "bridge0", "eth0", "eno1", "enp3s0", "wlan0", "wlp2s0", "en0", "ppp0", "wwan0", "bond0", "vlan10"} {
		if virtualBridgeIface(name) {
			t.Errorf("%s can be a real uplink (br0 is commonly the LAN bridge on a KVM host) — excluding it would break the same-LAN discovery this exists for", name)
		}
	}
}

// TestOwnAddrCandidateRejected is the receive-side defence, and it keeps working
// against peers on older builds that still advertise their bridges. We cannot
// see a peer's interface names, so we cannot tell whether the 192.168.122.1 it
// sent is an uplink or its libvirt bridge — but we do not need to. If we hold
// that address ourselves, dialing it reaches this very daemon, so it is
// worthless as a path to that peer whatever it is on their end.
func TestOwnAddrCandidateRejected(t *testing.T) {
	e, ns := testEngineWithNet(t)

	libvirt := netip.MustParseAddr("192.168.122.1")
	real := netip.MustParseAddr("192.168.55.3")
	own := map[netip.Addr]bool{libvirt: true} // we run libvirt too
	e.ownAddrs.Store(&own)

	e.addLocalCandidates(ns.spec.ID, "gn-mcfed", []netip.AddrPort{
		netip.AddrPortFrom(libvirt, 65432),
		netip.AddrPortFrom(real, 65432),
	})

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, s := range ns.seeds {
		if s.Addr() == libvirt {
			t.Fatalf("a peer's candidate at an address we hold ourselves must never be seeded — dialing it reaches this daemon, not the peer; seeds=%v", ns.seeds)
		}
	}
	var gotReal bool
	for _, s := range ns.seeds {
		if s.Addr() == real {
			gotReal = true
		}
	}
	if !gotReal {
		t.Fatalf("a genuine LAN candidate must still be seeded; seeds=%v", ns.seeds)
	}
}

// TestRefreshPopulatesOwnAddrsIncludingBridges: ownAddrs must record every
// address the host holds — including the bridge addresses we refuse to
// advertise. Recording only what we advertise would leave exactly the ambiguous
// addresses unguarded on receive, which is the whole point of the set.
func TestRefreshPopulatesOwnAddrsIncludingBridges(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", PrimaryPort: 65432})
	m := e.ownAddrs.Load()
	if m == nil {
		t.Fatal("refreshLocalCandidates must publish the host's own address set")
	}
	// Whatever this test host has, every advertised candidate must also appear
	// in ownAddrs — the advertised set is a subset of what we hold.
	for _, ep := range e.localEndpoints() {
		if !e.isOwnAddr(ep.Addr()) {
			t.Errorf("advertised candidate %s is not in ownAddrs; the set is meant to be a superset of what we advertise", ep)
		}
	}
}

// TestUpgradeSerializedPerNodeNotPerSeed: the unthrottled upgrade paths
// (explicit seeds since v370, host candidates since v381) are keyed by *seed* —
// but a peer routinely owns many seeds. gn-ionos2 has twelve configured TCP seed
// ports, plus its UDP endpoint, plus host candidates. All of them land in
// seedOwnerNeedsUpgrade independently, on every tick, with no
// directUpgradeInterval holding them back.
//
// So one relay-connected peer fired a dozen concurrent upgrade handshakes per
// second. Each success installed a fresh session and displaced the previous one
// — and a displaced session cannot simply be deleted, because its localIdx is a
// receive index the peer may still be addressing packets to (deleting it tears
// down a live path; twenty end-to-end tests say so). So they accumulated until
// pruneDead reaped them ~20s later, producing the observed storm: twenty-five
// identical "pruned dead session to 5f87d03fdff7b708" lines every five seconds,
// forever.
//
// One upgrade in flight per peer is all that is ever useful — they all reach the
// same node, and the first to land makes the rest redundant.
func TestUpgradeSerializedPerNodeNotPerSeed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	now := time.Now()

	// gn-ionos2, reachable only via relay, owning several seeds — as it does in
	// production via its many configured TCP ports.
	relay := &peerSession{nodeID: "gn-freebsd"}
	peer := &peerSession{nodeID: "gn-ionos2", relay: relay}
	seeds := []netip.AddrPort{
		netip.MustParseAddrPort("66.179.240.44:65432"),
		netip.MustParseAddrPort("66.179.240.44:443"),
		netip.MustParseAddrPort("66.179.240.44:23"),
		netip.MustParseAddrPort("66.179.240.44:79"),
	}
	ns.mu.Lock()
	ns.byNode["gn-ionos2"] = peer
	ns.explicitSeedNode["gn-ionos2"] = true // the unthrottled path
	for _, s := range seeds {
		ns.seedOwner[s] = "gn-ionos2"
	}
	ns.mu.Unlock()

	// One tick: every seed of this peer asks whether it should launch an upgrade.
	due := 0
	for _, s := range seeds {
		if e.seedOwnerNeedsUpgrade(ns, s, now) {
			due++
		}
	}
	if due != 1 {
		t.Fatalf("in one tick, %d of this peer's %d seeds launched an upgrade handshake; exactly 1 should — they all reach the same node, and each extra one installs a session that displaces the last and lingers until pruneDead reaps it", due, len(seeds))
	}

	// Still inside the gap: nothing more fires.
	for _, s := range seeds {
		if e.seedOwnerNeedsUpgrade(ns, s, now.Add(upgradeNodeInterval/2)) {
			t.Fatal("a second upgrade to the same peer fired inside upgradeNodeInterval")
		}
	}

	// Once the gap elapses, another seed gets its turn — the peer is still
	// relayed, so escaping the relay must keep being attempted.
	after := now.Add(upgradeNodeInterval + time.Second)
	due = 0
	for _, s := range seeds {
		if e.seedOwnerNeedsUpgrade(ns, s, after) {
			due++
		}
	}
	if due != 1 {
		t.Fatalf("after upgradeNodeInterval, exactly one seed should take the next turn; got %d", due)
	}
}

// TestUpgradeGateIsPerPeerNotGlobal: serializing must not let one relayed peer
// starve another. The gate is keyed by node id, so two different relayed peers
// each get their own upgrade in the same tick.
func TestUpgradeGateIsPerPeerNotGlobal(t *testing.T) {
	e, ns := testEngineWithNet(t)
	now := time.Now()
	relay := &peerSession{nodeID: "gn-freebsd"}

	a := netip.MustParseAddrPort("66.179.240.44:65432")
	b := netip.MustParseAddrPort("74.208.225.216:65432")
	ns.mu.Lock()
	ns.byNode["gn-ionos2"] = &peerSession{nodeID: "gn-ionos2", relay: relay}
	ns.byNode["gn-ionos3"] = &peerSession{nodeID: "gn-ionos3", relay: relay}
	ns.explicitSeedNode["gn-ionos2"] = true
	ns.explicitSeedNode["gn-ionos3"] = true
	ns.seedOwner[a] = "gn-ionos2"
	ns.seedOwner[b] = "gn-ionos3"
	ns.mu.Unlock()

	if !e.seedOwnerNeedsUpgrade(ns, a, now) {
		t.Fatal("first peer should get its upgrade")
	}
	if !e.seedOwnerNeedsUpgrade(ns, b, now) {
		t.Fatal("a different relayed peer must get its own upgrade in the same tick — the gate is per peer, not global")
	}
}

// TestCanSourceFamilyGatesUnreachableFamilies: a link with no IPv6 (a phone
// tether, most cellular data) cannot originate a single packet to a peer's IPv6
// endpoint or IPv6 host candidate. Every such send and dial is a guaranteed
// ENETUNREACH — and gravinet retried each one on every cycle, forever:
//
//	send: write udp6 [::]:65432->[fdf5:...]:65432: sendto: network is unreachable
//	tcp fallback dial [fdf5:...]:65432: connect: network is unreachable
//
// Dozens of pointless syscalls a tick, drowning the log and eating the dial
// budget the addresses that *can* work are competing for — exactly when a roam
// has left the node with the least room to waste.
func TestCanSourceFamilyGatesUnreachableFamilies(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", PrimaryPort: 65432})

	v4 := netip.MustParseAddr("192.168.5.108")
	v6 := netip.MustParseAddr("fdf5:168:5:0:7e58:6b00:6acb:47fe")

	// v4-only host, as on most cellular links.
	own := map[netip.Addr]bool{netip.MustParseAddr("10.83.82.7"): true}
	e.ownAddrs.Store(&own)
	e.haveV4.Store(true)
	e.haveV6.Store(false)

	if !e.canSourceFamily(v4) {
		t.Error("a v4 address must be dialable on a host that holds a v4 source")
	}
	if e.canSourceFamily(v6) {
		t.Error("a v6 address must not be dialed on a host with no v6 source — every attempt is a guaranteed ENETUNREACH")
	}

	// The roam gains IPv6: the very next refresh must pick it back up. This is
	// why the check is re-evaluated per tick rather than latched.
	e.haveV6.Store(true)
	if !e.canSourceFamily(v6) {
		t.Error("gaining a v6 source must re-enable v6 dialing within a tick")
	}
}

// TestCanSourceFamilyFailsOpenBeforeEnumeration: if we have not enumerated our
// own addresses yet, we must not refuse to dial. Failing closed on no evidence
// would wedge a node into dialing nothing at all — far worse than the wasted
// syscalls this exists to prevent.
func TestCanSourceFamilyFailsOpenBeforeEnumeration(t *testing.T) {
	e := &Engine{} // nothing enumerated, both haveV4/haveV6 false
	if !e.canSourceFamily(netip.MustParseAddr("192.168.5.108")) {
		t.Fatal("with no enumeration yet, must fail OPEN — refusing to dial on no evidence would wedge the node entirely")
	}
	if !e.canSourceFamily(netip.MustParseAddr("2001:db8::1")) {
		t.Fatal("with no enumeration yet, must fail OPEN for v6 too")
	}
	if e.canSourceFamily(netip.Addr{}) {
		t.Error("an invalid address is not sourceable in any family")
	}
}

// TestSeedInUnreachableFamilyIsSkipped: the guard has to actually be applied on
// the hot path — initSeedTick — not merely available.
func TestSeedInUnreachableFamilyIsSkipped(t *testing.T) {
	e, ns := testEngineWithNet(t)
	f := &fakeFallback{has: map[netip.AddrPort]bool{}}
	e.Attach(f)

	own := map[netip.Addr]bool{netip.MustParseAddr("10.83.82.7"): true}
	e.ownAddrs.Store(&own)
	e.haveV4.Store(true)
	e.haveV6.Store(false) // no IPv6 on this link

	v6seed := netip.MustParseAddrPort("[fdf5:168:5:0:7e58:6b00:6acb:47fe]:65432")
	ns.mu.Lock()
	ns.seeds = append(ns.seeds, v6seed)
	ns.seedBackoff[v6seed] = time.Now().Add(-time.Hour) // long past cooldown: would dial
	ns.mu.Unlock()

	e.initSeedTick(ns, v6seed, map[netip.AddrPort]time.Time{}, time.Now())
	time.Sleep(150 * time.Millisecond)

	if d := f.dials(); len(d) != 0 {
		t.Fatalf("a seed in an unsourceable family must not be dialed at all; got %v", d)
	}
	ns.mu.RLock()
	pending := len(ns.pending)
	ns.mu.RUnlock()
	if pending != 0 {
		t.Errorf("no handshake should have been planned for an unsourceable family; %d pending", pending)
	}
}
