package mesh

import (
	"net/netip"
	"testing"
)

// A configured "tcp://" seed is an operator-authored config entry in exactly
// the same sense a bare one is — the scheme picks which transport dials it,
// not how deliberate the address is. Marking only NetSpec.Seeds left every
// configured TCP seed looking dynamically-learned to install()'s stale-seed
// pruning, which then deleted it (and, critically, its seedOwner attribution)
// on every re-handshake while primeTCPSeeds re-added it unowned on the very
// next tick. An unowned seed is invisible to connectedToSeedOwner, so
// initSeedTick ran a full fresh handshake to it once a second forever, each
// one displacing the live session for that peer. Against a peer advertising a
// dozen extra listen ports that is a dozen session replacements per second,
// indefinitely, with the displaced sessions piling up until pruneDead reaped
// them. See v780.

func tcpSeedEngine(t *testing.T, udp, tcp []netip.AddrPort) (*Engine, *netState) {
	t.Helper()
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"),
		Subnet4:  netip.MustParsePrefix("10.0.0.0/24"),
		Seeds:    udp,
		TCPSeeds: tcp,
	}}})
	ns := e.netSnapshot()[1]
	if ns == nil {
		t.Fatal("network not created")
	}
	return e, ns
}

// TestConfiguredTCPSeedsAreExplicit: newNetState must mark both configured
// lists, not just the UDP one.
func TestConfiguredTCPSeedsAreExplicit(t *testing.T) {
	udp := netip.MustParseAddrPort("203.0.113.9:65432")
	tcp1 := netip.MustParseAddrPort("203.0.113.9:23")
	tcp2 := netip.MustParseAddrPort("203.0.113.9:443")

	_, ns := tcpSeedEngine(t, []netip.AddrPort{udp}, []netip.AddrPort{tcp1, tcp2})

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, s := range []netip.AddrPort{udp, tcp1, tcp2} {
		if !ns.explicitSeed[s] {
			t.Errorf("configured seed %s not marked explicit", s)
		}
	}
}

// TestInstallKeepsConfiguredTCPSeedOwner is the churn loop itself, in one
// test: a peer connected at one of its configured TCP ports must not have the
// *other* configured ports' owner attribution destroyed. Before the fix the
// prune deleted seedOwner for every sibling port, leaving them unowned, and
// connectedToSeedOwner then reported "not connected" for a peer that plainly
// was — which is what re-authorized a fresh handshake to each of them on the
// next tick.
func TestInstallKeepsConfiguredTCPSeedOwner(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.9")
	current := netip.AddrPortFrom(ip, 65432)
	sibling := netip.AddrPortFrom(ip, 23)

	e, ns := tcpSeedEngine(t, nil, []netip.AddrPort{current, sibling})

	// Both ports have completed a handshake at some point, so both carry the
	// owner attribution install() records (handshake_engine.go's
	// ns.seedOwner[p.endpoint] = pl.NodeID). Both are live dial candidates.
	ns.mu.Lock()
	ns.seeds = append(ns.seeds, current, sibling)
	ns.seedOwner[current] = "peer1"
	ns.seedOwner[sibling] = "peer1"
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.RLock()
	owner := ns.seedOwner[sibling]
	var present bool
	for _, s := range ns.seeds {
		if s == sibling {
			present = true
		}
	}
	ns.mu.RUnlock()

	if owner != "peer1" {
		t.Errorf("install() dropped the owner of configured TCP seed %s (got %q) — it will be re-handshaked every tick", sibling, owner)
	}
	if !present {
		t.Errorf("install() pruned configured TCP seed %s from the dial set", sibling)
	}
	if !e.connectedToSeedOwner(ns, sibling) {
		t.Error("connectedToSeedOwner does not recognize the sibling port's peer as connected — this is the re-handshake loop")
	}
}

// TestInstallStillPrunesUnconfiguredStaleSeeds: the fix must not blunt the
// pruning it narrows. A gossip-learned port for the same peer carries no
// explicit mark and is still a stale guess superseded by the real endpoint.
func TestInstallStillPrunesUnconfiguredStaleSeeds(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.9")
	current := netip.AddrPortFrom(ip, 65432)
	learned := netip.AddrPortFrom(ip, 40001)

	e, ns := tcpSeedEngine(t, nil, []netip.AddrPort{current})

	ns.mu.Lock()
	ns.seeds = append(ns.seeds, current, learned)
	ns.seedOwner[current] = "peer1"
	ns.seedOwner[learned] = "peer1"
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer1", endpoint: current, sess: testSession(t)}
	e.install(ns, ps)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if _, still := ns.seedOwner[learned]; still {
		t.Error("install() no longer prunes a gossip-learned stale seed")
	}
	for _, s := range ns.seeds {
		if s == learned {
			t.Error("gossip-learned stale seed survived the prune")
		}
	}
}

// TestAddTCPSeedMarksExplicit: ReloadRuntime's config-seed merge is the only
// caller, so everything reaching it came from the operator's config. Without
// the mark, whether a configured TCP seed was prune-exempt would depend on
// whether the daemon had been reloaded since it started.
func TestAddTCPSeedMarksExplicit(t *testing.T) {
	e, ns := tcpSeedEngine(t, nil, nil)
	seed := netip.MustParseAddrPort("203.0.113.9:513")

	e.addTCPSeed(ns.spec.ID, seed)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if !ns.explicitSeed[seed] {
		t.Error("addTCPSeed did not mark the reload-merged seed explicit")
	}
	var n int
	for _, s := range ns.tcpSeeds {
		if s == seed {
			n++
		}
	}
	if n != 1 {
		t.Errorf("tcpSeeds holds %d copies of the seed, want 1", n)
	}
}

// TestAddTCPSeedPromotesKnownOwner mirrors addSeed's handling of the same
// case: an address whose owner was already learned on an earlier connection
// promotes to explicitSeedNode now rather than waiting for a future
// attribution.
func TestAddTCPSeedPromotesKnownOwner(t *testing.T) {
	e, ns := tcpSeedEngine(t, nil, nil)
	seed := netip.MustParseAddrPort("203.0.113.9:513")

	ns.mu.Lock()
	ns.seedOwner[seed] = "peer1"
	ns.mu.Unlock()

	e.addTCPSeed(ns.spec.ID, seed)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if !ns.explicitSeedNode["peer1"] {
		t.Error("addTCPSeed did not promote the already-known owner to explicitSeedNode")
	}
}
