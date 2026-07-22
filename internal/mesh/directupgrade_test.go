package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestRelayedConnectionUpgradesToDirect is the direct empirical test of the
// gap this session's investigation found: once a peer is reached via relay,
// nothing ever re-evaluated whether a direct path had since become
// available — connectedTo/connectedToSeedOwner/connectedToNode all treat
// "any live session" as "nothing to do here," forever. This reproduces the
// exact shape reported (two peers that should be directly reachable stuck
// going through a relay) and confirms the fix: once the direct path is no
// longer blocked, A and B upgrade to a direct session within
// directUpgradeInterval, and B is reachable throughout — the relay is never
// required to stop working before the direct path takes over.
// TestSeedOwnerNeedsUpgradeThrottles is the fast, isolated counterpart to
// TestRelayedConnectionUpgradesToDirect: exercises seedOwnerNeedsUpgrade
// directly (no network, no timing waits) to confirm each of its cases —
// no owner, no session, direct session, relay session (first attempt vs.
// throttled vs. after the interval elapses) — without needing a full
// integration test for each one.
func TestSeedOwnerNeedsUpgradeThrottles(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	ns := e.network(1)
	seed := netip.MustParseAddrPort("203.0.113.5:65432")
	now := time.Now()

	if e.seedOwnerNeedsUpgrade(ns, seed, now) {
		t.Fatal("no seed owner attributed at all: should not need an upgrade attempt")
	}

	ns.mu.Lock()
	ns.seedOwner[seed] = "peer"
	ns.mu.Unlock()
	if e.seedOwnerNeedsUpgrade(ns, seed, now) {
		t.Fatal("owner attributed but no live session: should not need an upgrade attempt")
	}

	directPS := &peerSession{nodeID: "peer"}
	ns.mu.Lock()
	ns.byNode["peer"] = directPS
	ns.mu.Unlock()
	if e.seedOwnerNeedsUpgrade(ns, seed, now) {
		t.Fatal("owner already connected directly: should not need an upgrade attempt")
	}

	relayPS := &peerSession{nodeID: "relay"}
	directPS.relay = relayPS // now relayed, not direct
	if !e.seedOwnerNeedsUpgrade(ns, seed, now) {
		t.Fatal("owner connected only via relay, first check: should need an upgrade attempt")
	}
	if e.seedOwnerNeedsUpgrade(ns, seed, now.Add(time.Millisecond)) {
		t.Fatal("immediately after a recorded attempt: should be throttled, not need another")
	}
	if !e.seedOwnerNeedsUpgrade(ns, seed, now.Add(directUpgradeInterval+time.Second)) {
		t.Fatal("after directUpgradeInterval has elapsed: should need another attempt")
	}
}

func TestRelayedConnectionUpgradesToDirect(t *testing.T) {
	orig := directUpgradeInterval
	directUpgradeInterval = 500 * time.Millisecond // real default is 5m; too slow for a test
	defer func() { directUpgradeInterval = orig }()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const netID = uint64(0x09096)
	sb := newSwitchboard()

	addrA := netip.MustParseAddrPort("100.64.1.1:1")
	addrB := netip.MustParseAddrPort("100.64.1.2:1")
	addrR := netip.MustParseAddrPort("100.64.1.3:1")

	mk := func(name string, self netip.Addr, allowRelay bool, myAddr netip.AddrPort) (*Engine, *fakeDev) {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "r", Keys: ks, Dev: dev, Self4: self, AllowRelay: allowRelay}},
		})
		eng.Attach(sbSender{sb, myAddr})
		sb.register(myAddr, eng)
		eng.Start()
		return eng, dev
	}

	engA, devA := mk("A", netip.MustParseAddr("10.9.9.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.9.9.2"), false, addrB)
	engR, devR := mk("R", netip.MustParseAddr("10.9.9.3"), true, addrR)
	defer func() {
		devA.Close()
		devB.Close()
		devR.Close()
		for _, e := range []*Engine{engA, engB, engR} {
			e.Stop()
		}
	}()

	sb.block(addrA.Addr(), addrB.Addr())
	engA.AddSeed(netID, addrR)
	engB.AddSeed(netID, addrR)

	nsA, nsB := nsOf(engA, netID), nsOf(engB, netID)

	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsA, "B") && engB.connectedToNode(nsB, "A")
	}) {
		t.Fatalf("relayed session did not form: A->B=%v B->A=%v",
			engA.connectedToNode(nsA, "B"), engB.connectedToNode(nsB, "A"))
	}
	relayedOf := func(ns *netState, id string) bool {
		ns.mu.RLock()
		defer ns.mu.RUnlock()
		ps := ns.byNode[id]
		return ps != nil && ps.getRelay() != nil
	}
	if !relayedOf(nsA, "B") {
		t.Fatal("precondition failed: A's session to B should be relayed before the direct path opens")
	}

	// The direct path opens. Nothing tears the relayed session down —
	// upgrading is opportunistic, not forced.
	sb.unblock(addrA.Addr(), addrB.Addr())

	if !waitUntil(15*time.Second, func() bool {
		return !relayedOf(nsA, "B") && !relayedOf(nsB, "A")
	}) {
		t.Fatalf("did not upgrade to direct within the (shortened) directUpgradeInterval: A->B relayed=%v B->A relayed=%v",
			relayedOf(nsA, "B"), relayedOf(nsB, "A"))
	}

	// And B must have stayed reachable the entire time — this is the actual
	// point, not just "the relayed flag flipped." Send once more, now that
	// the session is direct, and confirm delivery.
	payload := []byte("post-upgrade-direct-payload")
	pkt := makeIPv4(netip.MustParseAddr("10.9.9.1"), netip.MustParseAddr("10.9.9.2"), payload)
	devA.in <- pkt
	select {
	case got := <-devB.out:
		if string(got) != string(pkt) {
			t.Fatalf("post-upgrade packet differs:\n got=%x\nwant=%x", got, pkt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("packet did not arrive after upgrading to direct")
	}
	t.Log("relayed session upgraded to direct, and remained reachable throughout")
}

// TestUpgradeAttemptAlsoTriesFallback is the fast, isolated counterpart to
// TestRelayedConnectionUpgradesToDirect, for the gap that test's own harness
// (a plain in-memory UDP switchboard, with no fallbackDialer at all) can't
// exercise: before this fix, seedOwnerNeedsUpgrade decided an upgrade
// attempt was due, but initLoop's upgrade branch only ever acted on that by
// calling planHandshake/e.send — the plain UDP path. Once a peer was
// relay-connected, that was the *only* remaining retry path (the backoff
// branch that normally calls ensureFallback is unreachable once
// connectedToSeedOwner is true), so a peer relayed because UDP genuinely
// doesn't reach it — up to and including UDP being turned off entirely
// (config.PrimaryPort == 0, the '-' port setting) — could never upgrade to a
// working TCP/TLS fallback path, even though the ordinary "not yet
// connected at all" branch already tries exactly that. This calls
// initSeedTick directly (no timers, no real transport) on a seed whose
// owner is relay-connected, and confirms ensureFallback now dials the
// fallback alongside the UDP attempt.
func TestUpgradeAttemptAlsoTriesFallback(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	e := NewEngine(Options{
		NodeID:          "self",
		TCPFallbackPort: 443,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Keys: ks, Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	f := &fakeFallback{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.netSnapshot()[1]
	if ns == nil {
		t.Fatal("network not created")
	}

	seed := netip.MustParseAddrPort("203.0.113.9:65432")
	fb := netip.MustParseAddrPort("203.0.113.9:443")

	// seed belongs to "peer", which is connected only via a relay — the
	// same precondition TestSeedOwnerNeedsUpgradeThrottles uses to confirm
	// seedOwnerNeedsUpgrade itself says an attempt is due.
	relayPS := &peerSession{nodeID: "relay"}
	peerPS := &peerSession{nodeID: "peer", relay: relayPS}
	ns.mu.Lock()
	ns.seedOwner[seed] = "peer"
	ns.byNode["peer"] = peerPS
	ns.mu.Unlock()

	e.initSeedTick(ns, seed, nil, time.Now())

	// The dial runs off a goroutine ensureFallback starts; wait for it, same
	// as TestEnsureFallbackDialsAndSeeds does for the ordinary (not yet
	// connected) case this mirrors.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if d := f.dials(); len(d) != 1 || d[0] != fb {
		t.Fatalf("initSeedTick on a relay-only owner due for an upgrade should have dialed the fallback %s; got dials=%v", fb, d)
	}
}

// TestSeedOwnerNeedsUpgradeSkipsThrottleForExplicitSeed is
// TestSeedOwnerNeedsUpgradeThrottles' counterpart for an operator-configured
// seed (explicitSeedNode): once its owner is known and relay-only, an
// upgrade attempt is due immediately and stays due — unlike a gossip-only
// peer, it is never gated by directUpgradeInterval. It's still paced, just
// by ns.seedBackoff (the same cadence a not-yet-connected peer gets) rather
// than being retried every single tick forever.
func TestSeedOwnerNeedsUpgradeSkipsThrottleForExplicitSeed(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	ns := e.network(1)
	seed := netip.MustParseAddrPort("203.0.113.6:65432")
	now := time.Now()

	relayPS := &peerSession{nodeID: "relay"}
	peerPS := &peerSession{nodeID: "peer", relay: relayPS}
	ns.mu.Lock()
	ns.seedOwner[seed] = "peer"
	ns.explicitSeedNode["peer"] = true
	ns.byNode["peer"] = peerPS
	ns.mu.Unlock()

	if !e.seedOwnerNeedsUpgrade(ns, seed, now) {
		t.Fatal("explicit seed, relay-only owner, no backoff yet: should need an upgrade attempt")
	}
	// It is NOT subject to directUpgradeInterval's 5 minutes — that is the point
	// of the explicit-seed path. It IS subject to upgradeNodeInterval, the short
	// per-peer gate that stops a peer's dozen seeds from all firing an upgrade at
	// once (see TestUpgradeSerializedPerNodeNotPerSeed). So step past that gate,
	// which is still orders of magnitude below the throttle a plain
	// gossip-learned endpoint would face here.
	soon := now.Add(upgradeNodeInterval + time.Second)
	if soon.Sub(now) >= directUpgradeInterval {
		t.Fatal("upgradeNodeInterval should be far shorter than directUpgradeInterval")
	}
	if !e.seedOwnerNeedsUpgrade(ns, seed, soon) {
		t.Fatal("explicit seed with no seedBackoff entry: should still need an upgrade attempt well inside directUpgradeInterval, not be throttled for 5 minutes")
	}
	// Simulate an exhausted handshake attempt cycle the way planHandshake
	// itself would (see planHandshake's own "exhausted all keys" branch).
	ns.mu.Lock()
	ns.seedBackoff[seed] = now.Add(seedRetryBackoff)
	ns.mu.Unlock()
	if e.seedOwnerNeedsUpgrade(ns, seed, now.Add(upgradeNodeInterval+time.Second)) {
		t.Fatal("explicit seed cooling down in ns.seedBackoff: should not need another attempt yet")
	}
	if !e.seedOwnerNeedsUpgrade(ns, seed, now.Add(seedRetryBackoff+time.Second)) {
		t.Fatal("explicit seed after its seedBackoff cooldown elapsed: should need another attempt")
	}
}

// TestAddExplicitSeedPromotesOwnerNode confirms addSeed promotes an address
// into explicitSeedNode regardless of which order the two facts arrive in —
// the address marked explicit before its owner is known (the common case: a
// config seed present from network construction, whose owner is only
// learned once gossip or a connection resolves it), or the owner already
// known before the address is (re)affirmed as an explicit seed on a later
// config reload.
func TestAddExplicitSeedPromotesOwnerNode(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	ns := e.network(1)

	addrA := netip.MustParseAddrPort("203.0.113.10:65432")
	e.AddExplicitSeed(1, addrA)
	e.AddSeedFor(1, addrA, "nodeA")
	ns.mu.RLock()
	gotA := ns.explicitSeedNode["nodeA"]
	ns.mu.RUnlock()
	if !gotA {
		t.Fatal("address marked explicit before its owner was known: owner should still be promoted")
	}

	addrB := netip.MustParseAddrPort("203.0.113.11:65432")
	e.AddSeedFor(1, addrB, "nodeB")
	e.AddExplicitSeed(1, addrB)
	ns.mu.RLock()
	gotB := ns.explicitSeedNode["nodeB"]
	ns.mu.RUnlock()
	if !gotB {
		t.Fatal("owner already known before the address was marked explicit: owner should still be promoted")
	}
}
