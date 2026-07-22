package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// testNode bundles an engine with its transport and device for multi-node tests.
type testNode struct {
	eng *Engine
	tr  *transport.Transport
	dev *fakeDev
}

func spinNode(t *testing.T, name string, netID uint64, key string, self netip.Addr) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets:     []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

func TestDistributedBan(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xBA11)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.8.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.8.0.2"))
	C := spinNode(t, "C", netID, key, netip.MustParseAddr("10.8.0.3"))
	all := []*testNode{A, B, C}
	defer func() {
		for _, n := range all {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	A.eng.AddSeed(netID, port(B))
	B.eng.AddSeed(netID, port(A))
	C.eng.AddSeed(netID, port(A)) // C learns B by gossip

	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 2 && B.eng.PeerCount(netID) == 2 && C.eng.PeerCount(netID) == 2
	}) {
		t.Fatalf("mesh did not form: A=%d B=%d C=%d", A.eng.PeerCount(netID), B.eng.PeerCount(netID), C.eng.PeerCount(netID))
	}

	// A bans C. The ban should flood to B, and both A and B should drop C.
	if err := A.eng.BanNode(netID, "C", "spam"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		bHasBan := false
		for _, b := range B.eng.ListBans(netID) {
			if b.Target == "C" && b.Origin == "A" && b.Hostname == "C" {
				bHasBan = true
			}
		}
		return bHasBan && A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatalf("ban did not propagate/enforce: A.peers=%d B.peers=%d B.bans=%v",
			A.eng.PeerCount(netID), B.eng.PeerCount(netID), B.eng.ListBans(netID))
	}

	// Only the originating node may unban: B must be refused.
	if err := B.eng.UnbanNode(netID, "C"); err == nil {
		t.Fatal("non-origin node B should not be able to unban C")
	}

	// A unbans; the removal should flood so B's ban clears.
	if err := A.eng.UnbanNode(netID, "C"); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		for _, b := range B.eng.ListBans(netID) {
			if b.Target == "C" {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("unban did not propagate to B: %v", B.eng.ListBans(netID))
	}

	// With unban-redial, the mesh re-forms quickly: A (and B, via the gossiped
	// unban) re-dial C's remembered endpoint instead of waiting out peerTimeout
	// (75s). A 25s bound passes only if that fast path works.
	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 2 && B.eng.PeerCount(netID) == 2 && C.eng.PeerCount(netID) == 2
	}) {
		t.Fatalf("mesh did not re-form after unban: A=%d B=%d C=%d",
			A.eng.PeerCount(netID), B.eng.PeerCount(netID), C.eng.PeerCount(netID))
	}
}

// TestBannedNodeSurvivesSweepDeadSeedsAndReconnectsOnUnban reproduces the bug
// report directly, from the banned node's own point of view — the side that
// actually needed a restart. B dials out to A as its seed (the common
// NAT'd-client topology: B can't be dialed into, so reconnection depends
// entirely on B's own seed list surviving). applyBan never touches a node's
// own seed list for itself (it only strips a banned peer's endpoint from
// *other* nodes' seed lists — see applyBan's early return for
// target==e.nodeID), so the only thing that could ever silently evict
// seedA from B's list is sweepDeadSeeds. Before this fix, sweepDeadSeeds
// judged only "is there a live session at this exact instant," which a ban
// guarantees false for as long as it's in effect on a seed old enough to be
// past deadSeedGrace (i.e. any established, long-running mesh member) — so
// once A banned B, the very next maintenance sweep on B would silently and
// permanently prune seedA, and nothing short of a restart (reloading seeds
// from config) ever brought it back, even after A lifted the ban. This ages
// the seed's first-seen timestamp past deadSeedGrace, bans B from A's side,
// runs the exact maintenance sweep that used to evict it (on B, the affected
// node), and confirms the seed survives and the mesh reconnects on its own
// once unbanned — no restart, no manual re-add.
func TestBannedNodeSurvivesSweepDeadSeedsAndReconnectsOnUnban(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xBA13)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.8.1.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.8.1.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	seedA := netip.AddrPortFrom(lo, uint16(A.tr.Port()))
	B.eng.AddSeed(netID, seedA) // B dials out to A; A never dials B directly

	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not connect")
	}

	// Age the seed past deadSeedGrace, as any long-standing mesh member's
	// seed naturally would be by the time an admin gets around to banning it.
	bns := B.eng.network(netID)
	bns.mu.Lock()
	bns.seedFirstSeen[seedA] = time.Now().Add(-2 * deadSeedGrace)
	hadEverConnected := bns.everConnected[seedA]
	bns.mu.Unlock()
	if !hadEverConnected {
		t.Fatal("everConnected should have been set by install() once B connected out to A")
	}

	if err := A.eng.BanNode(netID, "B", "test"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !waitUntil(5*time.Second, func() bool { return A.eng.PeerCount(netID) == 0 }) {
		t.Fatal("ban did not disconnect A from B")
	}

	// A never notifies B at the protocol level that it's banned — B's own
	// session to A only clears once pruneDead notices A has gone silent past
	// peerTimeout. Simulate that having already happened (rather than waiting
	// out the real timeout), so sweepDeadSeeds sees exactly the state it would
	// in production: no live session, seed old enough to be past its grace
	// period. Without this, B's stale-but-still-present session entry would
	// mask the bug this test exists to catch.
	bns.mu.Lock()
	delete(bns.byNode, "A")
	bns.mu.Unlock()

	// The exact call the maintenance loop makes every 5s, run directly on B
	// (the banned, affected node) so the test doesn't depend on real
	// wall-clock time: this is what used to silently strip seedA out of B's
	// own seed list while the ban was in effect.
	B.eng.sweepDeadSeeds(bns, time.Now())
	bns.mu.RLock()
	stillHasSeed := false
	for _, s := range bns.seeds {
		if s == seedA {
			stillHasSeed = true
		}
	}
	bns.mu.RUnlock()
	if !stillHasSeed {
		t.Fatal("B's seed for A must survive sweepDeadSeeds even while B is banned by A")
	}

	if err := A.eng.UnbanNode(netID, "B"); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A and B did not reconnect after unban — this is the bug: a restart would have been required")
	}
}

// TestEditBanNotes verifies that the node which issued a ban can edit its
// notes and have the change propagate mesh-wide, and that a non-origin node
// cannot edit someone else's ban.
func TestEditBanNotes(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xBA12)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.8.1.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.8.1.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	A.eng.AddSeed(netID, port(B))
	B.eng.AddSeed(netID, port(A))
	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatalf("mesh did not form: A=%d B=%d", A.eng.PeerCount(netID), B.eng.PeerCount(netID))
	}

	// A bans a (never-connected) target so the ban record exists to edit without
	// tearing down the A-B link.
	if err := A.eng.BanNode(netID, "ghost", "initial notes"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		for _, b := range B.eng.ListBans(netID) {
			if b.Target == "ghost" && b.Notes == "initial notes" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("initial ban did not propagate to B: %v", B.eng.ListBans(netID))
	}

	// A non-origin node cannot edit A's ban.
	if err := B.eng.EditBanNotes(netID, "ghost", "hijacked"); err == nil {
		t.Fatal("non-origin node B must not be able to edit A's ban notes")
	}

	// A edits its own ban's notes; the new notes must flood to B.
	if err := A.eng.EditBanNotes(netID, "ghost", "updated notes"); err != nil {
		t.Fatalf("edit notes: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		for _, b := range B.eng.ListBans(netID) {
			if b.Target == "ghost" && b.Notes == "updated notes" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("edited notes did not propagate to B: %v", B.eng.ListBans(netID))
	}

	// Editing a ban that doesn't exist here (wrong target) is an error.
	if err := A.eng.EditBanNotes(netID, "nonexistent", "x"); err == nil {
		t.Fatal("editing a non-existent ban should error")
	}
}
