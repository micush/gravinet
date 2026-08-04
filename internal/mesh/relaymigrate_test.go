package mesh

import (
	"bytes"
	"net/netip"
	"os"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestRelayedPathMigratesToABetterRelay is the end-to-end counterpart to
// relayrescore_test.go's decision tests. Those assert what relaySwitches
// decides; this asserts that acting on the decision actually works against real
// engines — that startRelayHandshake on a node we are *already* connected to
// replaces the session rather than racing install(), and that the peer stays
// reachable across the switch.
//
// That property was reasoned about by analogy with the relayed→direct upgrade
// path (which likewise handshakes while a session exists) rather than
// demonstrated, and it is the one part of this that a unit test on the cost
// model cannot reach.
//
// The switchboard is in-memory, so every path has an RTT near zero and no
// arrangement of engines can produce a genuine latency difference. The cost
// picture is therefore injected — rttNanos on the incumbent session and on each
// candidate, plus a gossiped far leg — and rescoreRelays is then called once
// directly rather than waited for. What that gives up is coverage of the
// maintLoop wiring (a single call site, covered by
// TestRescoreIsWiredIntoTheMaintenanceTick below); what it keeps is the real
// handshake, the real install(), and real traffic over the result, which is the
// part worth proving.
func TestRelayedPathMigratesToABetterRelay(t *testing.T) {
	// The incumbent must be old enough to be eligible, and this test injects
	// its own timings rather than waiting minutes for the real ones.
	origDwell, origInterval := relayRescoreDwell, relayRescoreInterval
	relayRescoreDwell, relayRescoreInterval = 0, 0
	defer func() { relayRescoreDwell, relayRescoreInterval = origDwell, origInterval }()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const netID = uint64(0x8E19)
	sb := newSwitchboard()

	addrA := netip.MustParseAddrPort("100.64.9.1:1")
	addrB := netip.MustParseAddrPort("100.64.9.2:1")
	addrR1 := netip.MustParseAddrPort("100.64.9.3:1")
	addrR2 := netip.MustParseAddrPort("100.64.9.4:1")

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

	engA, devA := mk("A", netip.MustParseAddr("10.8.1.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.8.1.2"), false, addrB)
	engR1, devR1 := mk("R1", netip.MustParseAddr("10.8.1.3"), true, addrR1)
	engR2, devR2 := mk("R2", netip.MustParseAddr("10.8.1.4"), true, addrR2)
	defer func() {
		devA.Close()
		devB.Close()
		devR1.Close()
		devR2.Close()
		for _, e := range []*Engine{engA, engB, engR1, engR2} {
			e.Stop()
		}
	}()

	// A and B can each reach both relays, but never each other — so A's path to
	// B is relayed and stays that way, with two candidates to choose between.
	sb.block(addrA.Addr(), addrB.Addr())
	for _, r := range []netip.AddrPort{addrR1, addrR2} {
		engA.AddSeed(netID, r)
		engB.AddSeed(netID, r)
	}

	nsA := nsOf(engA, netID)
	sessionOf := func(ns *netState, id string) *peerSession {
		ns.mu.RLock()
		defer ns.mu.RUnlock()
		return ns.byNode[id]
	}
	relayNameOf := func(ns *netState, id string) string {
		ps := sessionOf(ns, id)
		if ps == nil {
			return ""
		}
		if via := ps.getRelay(); via != nil {
			return via.nodeID
		}
		return "" // direct
	}

	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsA, "B") &&
			engA.connectedToNode(nsA, "R1") && engA.connectedToNode(nsA, "R2") &&
			relayNameOf(nsA, "B") != ""
	}) {
		t.Fatalf("setup did not converge: B=%v R1=%v R2=%v via=%q",
			engA.connectedToNode(nsA, "B"), engA.connectedToNode(nsA, "R1"),
			engA.connectedToNode(nsA, "R2"), relayNameOf(nsA, "B"))
	}

	incumbent := relayNameOf(nsA, "B")
	challenger := "R2"
	if incumbent == "R2" {
		challenger = "R1"
	}
	t.Logf("A reached B via %s; steering it to %s", incumbent, challenger)

	// Both relays must have gossiped that they know B, or neither is a
	// candidate at all and this test would pass for the wrong reason.
	if !waitUntil(20*time.Second, func() bool {
		c := sessionOf(nsA, challenger)
		return c != nil && c.reports("B")
	}) {
		t.Fatalf("%s never gossiped knowing B, so it was never a relay candidate", challenger)
	}

	// Inject the cost picture: the incumbent path measures badly end to end,
	// the incumbent relay is far, and the challenger is close to both us and B.
	// Real keepalives overwrite rttNanos every keepaliveInterval, so the
	// re-score is driven immediately after setting these rather than awaited.
	sessionOf(nsA, "B").rttNanos.Store(int64(400 * time.Millisecond))
	sessionOf(nsA, incumbent).rttNanos.Store(int64(350 * time.Millisecond))
	advertise(sessionOf(nsA, incumbent), "B", 300*time.Millisecond)
	sessionOf(nsA, challenger).rttNanos.Store(int64(10 * time.Millisecond))
	advertise(sessionOf(nsA, challenger), "B", 5*time.Millisecond)

	// Sanity-check the decision before asserting on the action, so a failure
	// below is unambiguously about the switch and not about the scoring.
	switches := engA.relaySwitches(nsA, time.Now())
	if len(switches) != 1 || switches[0].to.nodeID != challenger {
		t.Fatalf("expected one switch onto %s, got %+v", challenger, switches)
	}

	engA.rescoreRelays(nsA)

	if !waitUntil(20*time.Second, func() bool { return relayNameOf(nsA, "B") == challenger }) {
		t.Fatalf("path did not migrate: still via %q, want %q", relayNameOf(nsA, "B"), challenger)
	}

	// Still relayed (A and B genuinely cannot reach each other), just relayed
	// through the better candidate.
	if ps := sessionOf(nsA, "B"); ps == nil || ps.getRelay() == nil {
		t.Fatal("session to B should still be relayed, through the new relay")
	}

	// The actual point: B stayed reachable across the switch. Drain anything
	// still in flight from before the migration first, then send fresh.
	drain := time.After(500 * time.Millisecond)
drained:
	for {
		select {
		case <-devB.out:
		case <-drain:
			break drained
		}
	}
	payload := []byte("post-migration-payload")
	pkt := makeIPv4(netip.MustParseAddr("10.8.1.1"), netip.MustParseAddr("10.8.1.2"), payload)
	devA.in <- pkt
	select {
	case got := <-devB.out:
		if !bytes.Equal(got, pkt) {
			t.Fatalf("post-migration packet differs:\n got=%x\nwant=%x", got, pkt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("packet did not arrive after the relay switch")
	}
	t.Logf("relayed path migrated %s -> %s and stayed reachable", incumbent, challenger)
}

// The re-score only runs if something calls it. Asserted against the source
// because the maintenance loop's cadence (maintInterval) is a const and waiting
// on it in a test is slow and racy — this is the same reasoning behind the
// script-text assertions in internal/webadmin.
func TestRescoreIsWiredIntoTheMaintenanceTick(t *testing.T) {
	src, err := os.ReadFile("control.go")
	if err != nil {
		t.Skipf("source not present: %v", err)
	}
	if !bytes.Contains(src, []byte("e.rescoreRelays(ns)")) {
		t.Error("nothing calls rescoreRelays from the maintenance tick; relay paths would never be revisited")
	}
}
