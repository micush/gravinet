package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func retireEngine(t *testing.T) (*Engine, *netState) {
	t.Helper()
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	return e, e.netSnapshot()[1]
}

func seedSet(ns *netState) map[netip.AddrPort]bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := map[netip.AddrPort]bool{}
	for _, s := range ns.seeds {
		out[s] = true
	}
	return out
}

// Disabling a seed has to take the address out of the live dial set on the
// reload, not at the next restart. This is the half of the feature that is
// pure bookkeeping — the session teardown is covered separately below.
func TestReloadRetiresDisabledSeedLive(t *testing.T) {
	e, ns := retireEngine(t)
	keep := netip.MustParseAddrPort("198.51.100.9:51820")
	drop := netip.MustParseAddrPort("203.0.113.5:51820")

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{keep, drop}}); err != nil {
		t.Fatal(err)
	}
	if got := seedSet(ns); !got[keep] || !got[drop] {
		t.Fatalf("both seeds should be dialed first: %v", got)
	}

	// The operator disables one: it leaves the dial set, the other stays.
	if err := e.ReloadRuntime(1, NetSpec{ID: 1,
		Seeds:        []netip.AddrPort{keep},
		RetiredSeeds: []netip.AddrPort{drop},
	}); err != nil {
		t.Fatal(err)
	}
	got := seedSet(ns)
	if got[drop] {
		t.Error("a disabled seed must be removed from the live dial set without a restart")
	}
	if !got[keep] {
		t.Error("retiring one seed must not disturb the others")
	}

	// Re-enabling puts it straight back.
	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{keep, drop}}); err != nil {
		t.Fatal(err)
	}
	if !seedSet(ns)[drop] {
		t.Error("re-enabling a seed must return it to the dial set on the reload")
	}
}

// Retiring must clear the seed's bookkeeping along with the entry. If backoff
// survived, a seed disabled while it happened to be in a long retry backoff
// would come back enabled and then sit there not being dialed — the "enable
// it and nothing happens for minutes" bug, which is invisible because the
// address is present and looks correct.
func TestRetiredSeedClearsBackoffSoReEnableDialsPromptly(t *testing.T) {
	e, ns := retireEngine(t)
	s := netip.MustParseAddrPort("203.0.113.5:51820")
	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}

	ns.mu.Lock()
	ns.seedBackoff[s] = time.Now().Add(time.Hour)
	ns.seedFirstSeen[s] = time.Now().Add(time.Hour)
	ns.everConnected[s] = true
	ns.explicitSeed[s] = true
	ns.mu.Unlock()

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, RetiredSeeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.RLock()
	_, backoff := ns.seedBackoff[s]
	_, firstSeen := ns.seedFirstSeen[s]
	_, ever := ns.everConnected[s]
	_, explicit := ns.explicitSeed[s]
	ns.mu.RUnlock()
	if backoff || firstSeen || ever || explicit {
		t.Fatalf("retiring must clear the seed's bookkeeping: backoff=%v firstSeen=%v everConnected=%v explicit=%v",
			backoff, firstSeen, ever, explicit)
	}
}

// The point of the whole change: the tunnel standing on a disabled seed goes
// away immediately. The peer is found by owner attribution, which is what
// survives a peer roaming to a different endpoint after the handshake.
func TestRetiredSeedDisconnectsAttributedPeer(t *testing.T) {
	e, ns := retireEngine(t)
	s := netip.MustParseAddrPort("203.0.113.5:51820")
	roamed := netip.MustParseAddrPort("192.0.2.77:41000")

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.Lock()
	ns.seedOwner[s] = "peer1"
	// Endpoint deliberately not the seed address: the peer connected via the
	// seed and then roamed. Matching on endpoint alone would miss it.
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: roamed}
	ns.mu.Unlock()

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, RetiredSeeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.RLock()
	_, still := ns.byNode["peer1"]
	ns.mu.RUnlock()
	if still {
		t.Fatal("the session reached over a disabled seed must be torn down on the reload, not at the next restart")
	}
}

// The complement: a peer dialed straight from a seed before any gossip named
// its owner has no attribution, so it has to be found by its endpoint.
func TestRetiredSeedDisconnectsUnattributedPeerByEndpoint(t *testing.T) {
	e, ns := retireEngine(t)
	s := netip.MustParseAddrPort("203.0.113.5:51820")

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: s}
	ns.mu.Unlock()

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, RetiredSeeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.RLock()
	_, still := ns.byNode["peer1"]
	ns.mu.RUnlock()
	if still {
		t.Fatal("a session sitting on the disabled seed's own address must be torn down")
	}
}

// Retiring one seed must not disconnect peers reached over anything else.
// This is the blast-radius check: a seed's off switch governs that address,
// not the network.
func TestRetiredSeedLeavesOtherPeersAlone(t *testing.T) {
	e, ns := retireEngine(t)
	drop := netip.MustParseAddrPort("203.0.113.5:51820")
	other := netip.MustParseAddrPort("198.51.100.9:51820")

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{drop, other}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.Lock()
	ns.seedOwner[drop] = "peer1"
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: drop}
	ns.byNode["peer2"] = &peerSession{net: ns, nodeID: "peer2", endpoint: other}
	ns.byNode["peer3"] = &peerSession{net: ns, nodeID: "peer3", endpoint: netip.MustParseAddrPort("192.0.2.30:41000")}
	ns.mu.Unlock()

	if err := e.ReloadRuntime(1, NetSpec{ID: 1,
		Seeds:        []netip.AddrPort{other},
		RetiredSeeds: []netip.AddrPort{drop},
	}); err != nil {
		t.Fatal(err)
	}
	ns.mu.RLock()
	_, p1 := ns.byNode["peer1"]
	_, p2 := ns.byNode["peer2"]
	_, p3 := ns.byNode["peer3"]
	ns.mu.RUnlock()
	if p1 {
		t.Error("the peer on the disabled seed should be disconnected")
	}
	if !p2 {
		t.Error("a peer on a different seed must not be disconnected")
	}
	if !p3 {
		t.Error("a peer on a gossip-learned address must not be disconnected")
	}
}

// A reload carrying no retirements must not touch anything — the common case
// by far, and the one where an over-eager sweep would be most damaging.
func TestReloadWithoutRetirementsDisconnectsNobody(t *testing.T) {
	e, ns := retireEngine(t)
	s := netip.MustParseAddrPort("203.0.113.5:51820")
	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: s}
	ns.mu.Unlock()

	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{s}}); err != nil {
		t.Fatal(err)
	}
	ns.mu.RLock()
	_, still := ns.byNode["peer1"]
	ns.mu.RUnlock()
	if !still {
		t.Fatal("an ordinary reload must not disconnect a peer")
	}
}

// Disabling wins over a same-reload re-add: applyRetiredSeeds runs after the
// additive merge on purpose, so an address that appears in both lists (a seed
// disabled while the same address is also in PeerCache, before the caller's
// subtraction, or any future caller that gets the composition wrong) ends up
// out rather than in. Failing closed is the safer direction for an off switch.
func TestRetireBeatsSameReloadReAdd(t *testing.T) {
	e, ns := retireEngine(t)
	s := netip.MustParseAddrPort("203.0.113.5:51820")
	if err := e.ReloadRuntime(1, NetSpec{ID: 1,
		Seeds:        []netip.AddrPort{s},
		RetiredSeeds: []netip.AddrPort{s},
	}); err != nil {
		t.Fatal(err)
	}
	if seedSet(ns)[s] {
		t.Fatal("an address that is both added and retired in one reload must end up retired")
	}
}
