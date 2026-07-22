package mesh

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// testSession returns a working (non-nil) crypto session for tests that
// exercise install() directly: install() now sends the new peer an immediate
// peer-list gossip message (see peerListSig's doc comment in control.go), so
// a peerSession passed to it needs a real sess, not just the fields these
// tests actually care about.
func testSession(t *testing.T) *crypto.Session {
	t.Helper()
	sess, err := crypto.NewSession(crypto.DeriveSessionKeys(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), []byte("t"), true))
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func testEngineWithNet(t *testing.T) (*Engine, *netState) {
	t.Helper()
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	return e, e.netSnapshot()[1]
}

// TestConnectedToRecognizesResolvedFallback reproduces the core defect behind
// the connection-flapping loop: a peer reachable via a live session must be
// recognized as "connected to this seed" once ensureFallback has resolved and
// dialed a fallback address for it — even though that address's port differs
// from the seed's own port. Before the fix, connectedTo compared the exact
// address:port only, so this case returned false forever, and initLoop
// retried the already-connected peer on every tick, each retry orphaning the
// previous live session (visible in the logs as a repeating one-second cycle
// of "pruned dead session" / "tunnel up" / "udp appears blocked" for the same
// peers, indefinitely).
func TestConnectedToRecognizesResolvedFallback(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("203.0.113.9")
	seed := netip.AddrPortFrom(ip, 65432) // the configured (UDP-blocked) seed
	fb := netip.AddrPortFrom(ip, 443)     // what ensureFallback resolved and dialed for it

	// Nothing recorded yet — an unconnected seed must still report false.
	if e.connectedTo(ns, seed) {
		t.Fatal("connectedTo should be false before anything is connected")
	}

	// ensureFallback would have recorded this mapping when it dialed fb for
	// seed; simulate that directly rather than going through the real dialer.
	ns.mu.Lock()
	ns.seedFallback[seed] = fb
	ns.mu.Unlock()

	// Still false: knowing the resolved fallback address isn't the same as
	// being connected to it.
	if e.connectedTo(ns, seed) {
		t.Fatal("connectedTo should not report true just because a fallback address was resolved, before any session exists there")
	}

	// The peer connects, landing on the resolved fallback address.
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: fb}
	ns.mu.Unlock()

	if !e.connectedTo(ns, seed) {
		t.Fatal("connectedTo should recognize the peer as connected to seed via its resolved fallback address")
	}
}

// TestConnectedToDoesNotFalsePositiveAcrossPeersOnSameIP is the regression
// test for a broader (and wrong) version of the fix above: matching by IP
// alone, ignoring the port entirely, made connectedTo report true for *any*
// seed sharing an IP with *any* connected peer — which breaks any topology
// where multiple distinct peers share an address (several nodes behind one
// NAT gateway, or, as this test reproduces directly, several local peers all
// on 127.0.0.1 with different ports, exactly the shape of this package's own
// multi-node integration tests). The fix must be precise: only the specific
// address ensureFallback actually resolved for a given seed satisfies it.
func TestConnectedToDoesNotFalsePositiveAcrossPeersOnSameIP(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("127.0.0.1")

	// A live session to one peer, on this IP.
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: netip.AddrPortFrom(ip, 39899)}
	ns.mu.Unlock()

	// A completely different, unrelated seed at the same IP but a different
	// port — a genuinely different peer we are not connected to — must not
	// be reported as connected.
	otherSeed := netip.AddrPortFrom(ip, 59241)
	if e.connectedTo(ns, otherSeed) {
		t.Fatal("connectedTo must not treat a different peer sharing the same IP as already connected — this is the exact regression an IP-only match caused (see TestMeshFormation)")
	}
}

// TestInstallClearsBackoffForResolvedFallback mirrors the connectedTo test
// for the other half of the bug: once a peer connects via the address
// ensureFallback resolved for a backed-off seed, that seed's backoff entry
// must be cleared, or initLoop keeps treating it as still-failing and keeps
// calling ensureFallback every tick even though the peer is already up.
// Before the fix, install() deleted ns.seedBackoff keyed by ps.endpoint (the
// fallback address), which was never the key the entry was actually stored
// under (that's the original seed address), making the delete a silent no-op.
func TestInstallClearsBackoffForResolvedFallback(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("203.0.113.9")
	seed := netip.AddrPortFrom(ip, 65432)
	fb := netip.AddrPortFrom(ip, 443)

	ns.mu.Lock()
	ns.seedBackoff[seed] = time.Now().Add(seedRetryBackoff)
	ns.seedFallback[seed] = fb
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: fb, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.Lock()
	_, stillBackedOff := ns.seedBackoff[seed]
	ns.mu.Unlock()
	if stillBackedOff {
		t.Fatal("install() should clear the seed's backoff entry once the peer is reachable via its resolved fallback address")
	}
}

// TestInstallBackoffClearDoesNotAffectUnrelatedSeeds checks the fix is scoped
// correctly: a backoff entry for an unrelated seed (different IP, and not
// mapped to the installed session's address via seedFallback) must survive.
func TestInstallBackoffClearDoesNotAffectUnrelatedSeeds(t *testing.T) {
	e, ns := testEngineWithNet(t)
	unrelated := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.20"), 65432)
	ns.mu.Lock()
	ns.seedBackoff[unrelated] = time.Now().Add(seedRetryBackoff)
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), 443), sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.Lock()
	_, stillBackedOff := ns.seedBackoff[unrelated]
	ns.mu.Unlock()
	if !stillBackedOff {
		t.Fatal("install() cleared an unrelated seed's backoff entry")
	}
}

// TestInstallPrunesOwnedStaleSeeds reproduces the unbounded seed-list growth
// this fix addresses: a peer behind a NAT that rotates its source port
// accumulates a new seed entry every time a different peer gossips its
// latest observed endpoint (AddSeedFor records the owner at that point), and
// nothing ever removed the stale ones — left unchecked, initLoop keeps
// re-attempting every historical port forever. Once the node connects,
// install() should prune every seed positively attributed to it (via
// seedOwner) other than its current endpoint, and clean up their
// backoff/fallback bookkeeping too.
func TestInstallPrunesOwnedStaleSeeds(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("203.0.113.9")
	stale1 := netip.AddrPortFrom(ip, 40001)
	stale2 := netip.AddrPortFrom(ip, 40002)
	current := netip.AddrPortFrom(ip, 443)

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, stale1, stale2, current)
	ns.seedOwner[stale1] = "peer1"
	ns.seedOwner[stale2] = "peer1"
	ns.seedOwner[current] = "peer1"
	ns.seedBackoff[stale1] = time.Now().Add(seedRetryBackoff)
	ns.seedFallback[stale2] = current
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, s := range ns.seeds {
		if s == stale1 || s == stale2 {
			t.Fatalf("stale owned seed %s should have been pruned; seeds=%v", s, ns.seeds)
		}
	}
	found := false
	for _, s := range ns.seeds {
		if s == current {
			found = true
		}
	}
	if !found {
		t.Fatalf("current endpoint should remain in seeds; seeds=%v", ns.seeds)
	}
	if _, ok := ns.seedOwner[stale1]; ok {
		t.Error("seedOwner for pruned stale1 should be cleaned up")
	}
	if _, ok := ns.seedBackoff[stale1]; ok {
		t.Error("seedBackoff for pruned stale1 should be cleaned up")
	}
	if _, ok := ns.seedFallback[stale2]; ok {
		t.Error("seedFallback for pruned stale2 should be cleaned up")
	}
}

// TestInstallNeverPrunesExplicitSeeds is the roaming regression: ns.seeds is
// the only list initLoop dials, and install()'s pruning couldn't tell an
// operator-configured seed (NetSpec.Seeds) from a gossip-learned endpoint. So
// a configured seed got dropped like any other the moment gossip attributed
// it to a node (AddSeedFor) and that node was later seen at a different
// endpoint — a NAT rebind or roam on its end, which over a long uptime
// happens to every peer eventually. Invisible while the local node stays put;
// catastrophic the moment it changes networks (Wi-Fi → cellular), because
// every remaining (learned) endpoint is unreachable from the new underlay,
// pruneDead clears the sessions after peerTimeout, and initLoop is left
// re-dialing a list that can no longer answer — with the operator's own
// reachable anchors long since deleted. The node then stays fully dead until
// restart. The unbounded-growth problem the pruning exists for is purely a
// gossip-learned one (NetSpec.Seeds never grows on its own), so an explicit
// seed must be pinned — while a gossip-learned stale entry for the same node,
// on the same IP, must still be pruned exactly as before.
func TestInstallNeverPrunesExplicitSeeds(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("203.0.113.9")
	configured := netip.AddrPortFrom(ip, 65432) // operator-configured seed
	learned := netip.AddrPortFrom(ip, 40001)    // gossip-learned, same node, same IP
	current := netip.AddrPortFrom(ip, 40002)    // where the node is actually seen now

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, configured, learned, current)
	ns.explicitSeed[configured] = true // as AddExplicitSeed / newNetState would
	ns.seedOwner[configured] = "peer1" // gossip attributed it to peer1 at some point
	ns.seedOwner[learned] = "peer1"
	ns.seedOwner[current] = "peer1"
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.Lock()
	defer ns.mu.Unlock()
	var keptConfigured, keptLearned bool
	for _, s := range ns.seeds {
		switch s {
		case configured:
			keptConfigured = true
		case learned:
			keptLearned = true
		}
	}
	if !keptConfigured {
		t.Fatalf("operator-configured seed %s must never be pruned — it is the anchor the node needs to re-find the mesh after its own network changes; seeds=%v", configured, ns.seeds)
	}
	if keptLearned {
		t.Fatalf("gossip-learned stale seed %s should still be pruned as before; seeds=%v", learned, ns.seeds)
	}
	if ns.seedOwner[configured] != "peer1" {
		t.Error("a pinned explicit seed should keep its owner attribution, not have it deleted")
	}
}

// TestInstallRelayedSessionDoesNotPruneSeeds is the bug that made host
// candidates useless for exactly the peers they exist to help. onRelay
// dispatches an inner packet with a zero netip.AddrPort — the source we
// actually observed was the relay, not the peer — so a relayed session's
// ps.endpoint is invalid. install()'s stale-seed pruning then asks "is this
// seed something other than the peer's current endpoint?", which is trivially
// true for every seed when there IS no current endpoint. So a relayed install
// deleted every candidate it could attribute to that node: its gossip-learned
// endpoints and its host candidates alike — the LAN addresses whose entire
// purpose is upgrading a relayed peer to direct. The peer was then guaranteed
// to stay relayed, since the upgrade path had nothing left to dial. Pruning
// must only happen when there's a real endpoint doing the superseding.
func TestInstallRelayedSessionDoesNotPruneSeeds(t *testing.T) {
	e, ns := testEngineWithNet(t)
	lan := netip.MustParseAddrPort("192.168.55.1:65432")     // host candidate
	observed := netip.MustParseAddrPort("203.0.113.1:40001") // gossip-learned endpoint

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, lan, observed)
	ns.seedOwner[lan] = "gn-cush1"
	ns.seedOwner[observed] = "gn-cush1"
	ns.mu.Unlock()

	// A relayed session: no observed endpoint (zero AddrPort), reached via relay.
	relay := &peerSession{net: ns, nodeID: "gn-win11", sess: testSession(t)}
	ps := &peerSession{net: ns, nodeID: "gn-cush1", relay: relay, sess: testSession(t)}
	if ps.endpoint.IsValid() {
		t.Fatal("precondition: a relayed session should have no observed endpoint")
	}
	e.install(ns, ps)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	var keptLAN, keptObserved bool
	for _, s := range ns.seeds {
		switch s {
		case lan:
			keptLAN = true
		case observed:
			keptObserved = true
		}
	}
	if !keptLAN {
		t.Errorf("a relayed install must not prune the peer's host candidate %s — it is precisely what the direct-upgrade path needs to dial; seeds=%v", lan, ns.seeds)
	}
	if !keptObserved {
		t.Errorf("a relayed install must not prune the peer's observed endpoint %s either; seeds=%v", observed, ns.seeds)
	}
	if ns.seedOwner[lan] != "gn-cush1" || ns.seedOwner[observed] != "gn-cush1" {
		t.Error("owner attribution should survive a relayed install")
	}
	// A relayed session proves nothing about any underlay address being
	// reachable, so it must not mark the zero endpoint as everConnected —
	// that would make sweepDeadSeeds' "has this ever worked" check meaningless.
	if ns.everConnected[netip.AddrPort{}] {
		t.Error("a relayed install must not record the zero endpoint as everConnected")
	}
}

// TestInstallNeverPrunesHostCandidates is the churn the production logs caught:
// cush2 logged "learned host candidate X for peer Y" for the *same* address
// twice, three seconds apart. addSeed dedups, so a candidate can only be
// learned as new again if it was deleted in between — and the deleter was
// install()'s stale-seed prune. Its premise is "this seed is a stale guess at
// where the peer is, superseded by where it actually is." That is exactly false
// for a host candidate, which is *by definition* an address other than the
// observed endpoint (that's the whole point of it), owned by that node, and not
// an explicit seed — so the prune matched every single one. Every re-handshake
// deleted the peer's LAN addresses moments after they were learned; gossip
// re-added them; the next handshake deleted them again. Nothing could ever dial
// an address that never survived a tick. v376 gated the prune on a valid
// endpoint, which fixed this for *relayed* peers only — a direct peer has a
// perfectly valid endpoint and kept right on eating its own candidates.
func TestInstallNeverPrunesHostCandidates(t *testing.T) {
	e, ns := testEngineWithNet(t)
	current := netip.MustParseAddrPort("192.168.5.108:65432") // observed endpoint
	lan := netip.MustParseAddrPort("192.168.55.3:65432")      // host candidate
	stale := netip.MustParseAddrPort("192.168.5.108:40001")   // genuinely stale, must still go

	// As addLocalCandidates would record it.
	e.addLocalCandidates(ns.spec.ID, "gn-cush1", []netip.AddrPort{lan})
	ns.mu.Lock()
	ns.seeds = append(ns.seeds, current, stale)
	ns.seedOwner[current] = "gn-cush1"
	ns.seedOwner[stale] = "gn-cush1"
	ns.mu.Unlock()

	// A direct session — valid endpoint, so the prune genuinely runs.
	ps := &peerSession{net: ns, nodeID: "gn-cush1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	var keptLAN, keptStale bool
	for _, s := range ns.seeds {
		switch s {
		case lan:
			keptLAN = true
		case stale:
			keptStale = true
		}
	}
	if !keptLAN {
		t.Fatalf("a host candidate must survive its owner's install — it is never the observed endpoint, so the prune matches it every time and it can never be dialed; seeds=%v", ns.seeds)
	}
	if keptStale {
		t.Errorf("a genuinely stale observed endpoint should still be pruned as before; seeds=%v", ns.seeds)
	}
	if ns.seedOwner[lan] != "gn-cush1" {
		t.Error("a host candidate should keep its owner attribution across install")
	}
}

// TestInstallDoesNotPruneUnattributedOrOtherNodesSeeds is the same regression
// class as TestConnectedToDoesNotFalsePositiveAcrossPeersOnSameIP: pruning
// must never touch a seed with no recorded owner, or one attributed to a
// different node — even if it shares an IP with the newly-connected peer —
// since several distinct peers can share an address (e.g. behind one NAT
// gateway).
func TestInstallDoesNotPruneUnattributedOrOtherNodesSeeds(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ip := netip.MustParseAddr("203.0.113.9")
	unattributed := netip.AddrPortFrom(ip, 40001) // no seedOwner entry at all
	othersNode := netip.AddrPortFrom(ip, 40002)   // owned by a different node
	current := netip.AddrPortFrom(ip, 443)

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, unattributed, othersNode, current)
	ns.seedOwner[othersNode] = "peer2" // a different peer, sharing this IP
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.Lock()
	defer ns.mu.Unlock()
	haveUnattributed, haveOthers := false, false
	for _, s := range ns.seeds {
		if s == unattributed {
			haveUnattributed = true
		}
		if s == othersNode {
			haveOthers = true
		}
	}
	if !haveUnattributed {
		t.Fatal("a seed with no recorded owner must survive install() — pruning must never guess by IP")
	}
	if !haveOthers {
		t.Fatal("a seed owned by a different node must survive install(), even sharing an IP with the connected peer")
	}
}

// TestSweepDeadSeedsRemovesNeverConnected reproduces the actual gap this
// closes: a seed that's never once connected, and has been in ns.seeds well
// past its grace period, would otherwise be retried — and logged — for as
// long as the process stays up, restart or not. Confirms it's removed along
// with all its associated bookkeeping (backoff, fallback mapping, owner).
func TestSweepDeadSeedsRemovesNeverConnected(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.0.2.9:65432")

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, seed)
	ns.seedFirstSeen[seed] = time.Now().Add(-2 * deadSeedGrace) // well past grace
	ns.seedBackoff[seed] = time.Now().Add(seedRetryBackoff)
	ns.seedFallback[seed] = netip.MustParseAddrPort("192.0.2.9:443")
	ns.seedOwner[seed] = "peer1"
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, s := range ns.seeds {
		if s == seed {
			t.Fatalf("dead seed should have been removed; seeds=%v", ns.seeds)
		}
	}
	if _, ok := ns.seedFirstSeen[seed]; ok {
		t.Error("seedFirstSeen for removed seed should be cleaned up")
	}
	if _, ok := ns.seedBackoff[seed]; ok {
		t.Error("seedBackoff for removed seed should be cleaned up")
	}
	if _, ok := ns.seedFallback[seed]; ok {
		t.Error("seedFallback for removed seed should be cleaned up")
	}
	if _, ok := ns.seedOwner[seed]; ok {
		t.Error("seedOwner for removed seed should be cleaned up")
	}
}

// TestSweepDeadSeedsNeverEvictsExplicitSeed is the cold-start counterpart to
// TestInstallNeverPrunesExplicitSeeds. everConnected only ever gets set by a
// *successful* session, so a seed that has never once worked in this process
// is otherwise fair game for eviction — right for a gossip-learned address
// (it was only ever a guess, and will simply be re-learned if still valid),
// badly wrong for a configured one. A daemon that cold-starts while its
// configured seeds happen to be unreachable — booting on cellular, before
// the VPN or Wi-Fi is up, during an upstream outage — would evict them all
// after deadSeedGrace and then never retry them again, sitting permanently
// dead with an empty seed list even once the network came back, with nothing
// able to re-add them (unlike a learned entry, which gossip can). Nothing
// short of a config reload or restart would recover it. An explicit seed must
// survive indefinitely; an otherwise-identical learned one must still be
// evicted exactly as before.
func TestSweepDeadSeedsNeverEvictsExplicitSeed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	configured := netip.MustParseAddrPort("192.0.2.10:65432") // operator-configured
	learned := netip.MustParseAddrPort("192.0.2.11:65432")    // gossip-learned
	past := time.Now().Add(-2 * deadSeedGrace)                // both well past grace

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, configured, learned)
	ns.explicitSeed[configured] = true // as AddExplicitSeed / newNetState would
	ns.seedFirstSeen[configured] = past
	ns.seedFirstSeen[learned] = past
	// Neither has ever connected: no everConnected entry for either.
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	var keptConfigured, keptLearned bool
	for _, s := range ns.seeds {
		switch s {
		case configured:
			keptConfigured = true
		case learned:
			keptLearned = true
		}
	}
	if !keptConfigured {
		t.Fatalf("an operator-configured seed must never be evicted, even having never connected — nothing else can re-add it, and it is the node's only way back onto the mesh; seeds=%v", ns.seeds)
	}
	if keptLearned {
		t.Fatalf("a gossip-learned seed that never connected should still be evicted as before; seeds=%v", ns.seeds)
	}
	if _, ok := ns.seedFirstSeen[configured]; !ok {
		t.Error("a pinned explicit seed should keep its seedFirstSeen bookkeeping, not have it deleted")
	}
}

// TestSweepDeadSeedsProtectsWithinGrace: a seed that hasn't had deadSeedGrace
// worth of chances yet must survive, regardless of connection status —
// mirrors peer_cache's own grace-period protection for the same reason
// (routine downtime, or simply "hasn't had time to try yet," isn't
// permanent staleness).
func TestSweepDeadSeedsProtectsWithinGrace(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.0.2.9:65432")

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, seed)
	ns.seedFirstSeen[seed] = time.Now() // just added
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	found := false
	for _, s := range ns.seeds {
		if s == seed {
			found = true
		}
	}
	if !found {
		t.Fatal("seed within its grace period should not have been removed")
	}
}

// TestSweepDeadSeedsProtectsConnectedSeed: a seed with a live session at its
// exact address must never be removed, no matter how long it's been around.
func TestSweepDeadSeedsProtectsConnectedSeed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.0.2.9:65432")

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, seed)
	ns.seedFirstSeen[seed] = time.Now().Add(-2 * deadSeedGrace)
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: seed}
	ns.everConnected[seed] = true // what install() sets on a real connection
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	found := false
	for _, s := range ns.seeds {
		if s == seed {
			found = true
		}
	}
	if !found {
		t.Fatal("a connected seed should never be removed")
	}
}

// TestSweepDeadSeedsProtectsConnectedViaFallback: the same protection, but
// via the resolved-fallback path (a peer whose live session is on a
// different port than the seed being evaluated) — the exact scenario that
// motivated tcpPortForEndpoint's IP-based port discovery.
func TestSweepDeadSeedsProtectsConnectedViaFallback(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.0.2.9:65432")
	fb := netip.MustParseAddrPort("192.0.2.9:443")

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, seed)
	ns.seedFirstSeen[seed] = time.Now().Add(-2 * deadSeedGrace)
	ns.seedFallback[seed] = fb
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: fb}
	ns.everConnected[fb] = true // what install() sets on a real connection via fallback
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	found := false
	for _, s := range ns.seeds {
		if s == seed {
			found = true
		}
	}
	if !found {
		t.Fatal("a seed connected via its resolved fallback address should never be removed")
	}
}

// TestSweepDeadSeedsProtectsPreviouslyConnectedSeed reproduces the actual
// bug report this fix closes: a seed that connected fine in the past but is
// currently unreachable — a banned peer being the most reliable way to
// produce this, since the ban rejects every reconnect attempt for as long
// as it's in effect, guaranteeing no live session for the whole duration —
// must survive sweepDeadSeeds no matter how long the current gap lasts, so
// the peer reconnects on its own once the ban lifts instead of needing a
// restart to reload the seed from config. everConnected (set once, by
// install(), and never cleared) is exactly what makes this distinguishable
// from TestSweepDeadSeedsRemovesNeverConnected's "truly never worked" case
// below.
func TestSweepDeadSeedsProtectsPreviouslyConnectedSeed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	seed := netip.MustParseAddrPort("192.0.2.9:65432")

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, seed)
	// The seed has been around far longer than deadSeedGrace (an established,
	// long-running mesh member, not a fresh entry), and connected successfully
	// at some point in the past — but there is no live session right now: no
	// entry in ns.byNode at all, exactly what a banned (or merely offline)
	// peer looks like from this node's point of view.
	ns.seedFirstSeen[seed] = time.Now().Add(-2 * deadSeedGrace)
	ns.everConnected[seed] = true
	ns.mu.Unlock()

	e.sweepDeadSeeds(ns, time.Now())

	ns.mu.Lock()
	defer ns.mu.Unlock()
	found := false
	for _, s := range ns.seeds {
		if s == seed {
			found = true
		}
	}
	if !found {
		t.Fatal("a seed that has connected before must survive a sustained current outage (e.g. a ban) — it should never need a restart to be retried again")
	}
	if _, ok := ns.seedBackoff[seed]; ok {
		// Not part of this test's setup, but guard against a regression that
		// starts clearing bookkeeping for surviving seeds too.
		t.Error("surviving seed's bookkeeping should be untouched")
	}
}

// TestOutboundHandshakeAttributesSeedOwner is the integration-level
// regression test for the gap TestConnectedToRecognizesResolvedFallback and
// friends didn't cover: a *plain* statically-configured UDP seed (added via
// AddSeed, exactly like every seed loaded from the config file at startup —
// see cmd/gravinet's buildOneNetSpec/resolveSeeds feeding NetSpec.Seeds
// straight into newNetState, never through addSeed) never got a seedOwner
// entry at all, only seeds added later via AddSeedFor (control.go's gossip
// re-dial hint, or a live reload) did. That left connectedToSeedOwner unable
// to recognize the single most common case in the whole system: a config
// seed that connects fine but whose live session endpoint doesn't exactly
// equal the literal seed address on every packet (any NAT/PAT on either
// side). initLoop then re-dialed and re-handshook that peer every tick,
// forever — the once-a-second "tunnel up" churn for a subset of peers.
//
// This connects two real nodes over loopback via a plain AddSeed (no owner),
// and asserts that once the handshake completes, the seed is attributed to
// the peer's node id — which is what lets connectedToSeedOwner, and hence
// initLoop, recognize the peer as already connected from then on.
func TestOutboundHandshakeAttributesSeedOwner(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x5EED)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	seed := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(B.tr.Port()))
	A.eng.AddSeed(netID, seed) // plain AddSeed — no owner, exactly like a config-file seed

	if !waitUntil(8*time.Second, func() bool { return len(A.eng.PeerEndpoints(netID)) > 0 }) {
		t.Fatal("A never connected to B via the seed")
	}

	ns := A.eng.netSnapshot()[netID]
	ns.mu.RLock()
	owner, ok := ns.seedOwner[seed]
	ns.mu.RUnlock()
	if !ok || owner != "B" {
		t.Fatalf("seed %s should be attributed to B after a successful handshake, got owner=%q ok=%v", seed, owner, ok)
	}
	if !A.eng.connectedToSeedOwner(ns, seed) {
		t.Fatal("connectedToSeedOwner should recognize the seed as already connected, preventing initLoop from re-dialing it every tick")
	}
}
