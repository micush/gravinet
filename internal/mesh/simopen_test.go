package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// Two nodes that seed each other both dial, so before v803 both handshakes
// completed and each side kept two sessions for the same peer: one it
// initiated, one it answered. Nothing resolved that, and the consequences ran
// through several releases:
//
//   - ns.byNode holds whichever installed last, while the peer keeps sending to
//     the other index, so a "superseded" session is load-bearing (v802).
//   - Field bundles showed 24 sessions for a single peer reaped in one second.
//   - Reaping duplicates on a short grace broke path MTU discovery outright,
//     because the pair is asymmetric (v802, reverted).
//
// The fix resolves it the way a simultaneous TCP open is resolved: an ordering
// both ends compute identically. Smaller node id wins the initiator role.

func simOpenPair(t *testing.T, netID uint64, idA, idB string) (*Engine, *Engine, func()) {
	t.Helper()
	key, _ := crypto.GenerateKey()
	mk := func(node string, dev *fakeDev, self netip.Addr) (*Engine, *transport.Transport) {
		ks, _ := crypto.NewKeySet([]string{key})
		eng := NewEngine(Options{NodeID: node, Hostname: node,
			Nets: []NetSpec{{ID: netID, Name: "t", Keys: ks, Dev: dev, Self4: self}}})
		tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatalf("transport: %v", err)
		}
		eng.Attach(tr)
		eng.Start()
		return eng, tr
	}
	devA, devB := newFakeDev("a"), newFakeDev("b")
	engA, trA := mk(idA, devA, netip.MustParseAddr("10.70.0.1"))
	engB, trB := mk(idB, devB, netip.MustParseAddr("10.70.0.2"))
	lo := netip.MustParseAddr("127.0.0.1")
	// Both directions, deliberately: this is the configuration that produces
	// the simultaneous open.
	engA.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trB.Port())))
	engB.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trA.Port())))
	cleanup := func() {
		devA.Close()
		devB.Close()
		engA.Stop()
		engB.Stop()
		trA.Close()
		trB.Close()
	}
	if !waitUntil(15*time.Second, func() bool { return engA.SessionCount() > 0 && engB.SessionCount() > 0 }) {
		cleanup()
		t.Fatal("handshake did not complete")
	}
	return engA, engB, cleanup
}

func sessionCount(e *Engine) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.sessions)
}

// The headline: mutual seeding must converge on exactly one session per side.
func TestMutualSeedingConvergesOnOneSession(t *testing.T) {
	engA, engB, done := simOpenPair(t, 0x51F0, "A", "B")
	defer done()

	// Give both sides several maintenance ticks to settle and to prove they do
	// not drift back to two.
	settled := waitUntil(20*time.Second, func() bool {
		return sessionCount(engA) == 1 && sessionCount(engB) == 1
	})
	if !settled {
		t.Fatalf("did not converge: A=%d sessions, B=%d sessions (want 1 each)",
			sessionCount(engA), sessionCount(engB))
	}
	// Hold, so a later re-dial cannot quietly re-introduce the duplicate.
	time.Sleep(12 * time.Second)
	if a, b := sessionCount(engA), sessionCount(engB); a != 1 || b != 1 {
		t.Fatalf("drifted back to duplicates: A=%d, B=%d", a, b)
	}
}

// Convergence must not cost connectivity: exactly one session, and it works.
func TestConvergedSessionCarriesTraffic(t *testing.T) {
	engA, engB, done := simOpenPair(t, 0x51F1, "A", "B")
	defer done()
	if !waitUntil(20*time.Second, func() bool {
		return sessionCount(engA) == 1 && sessionCount(engB) == 1
	}) {
		t.Fatalf("did not converge: A=%d B=%d", sessionCount(engA), sessionCount(engB))
	}
	nsA := nsOf(engA, 0x51F1)
	if !engA.connectedToNode(nsA, "B") {
		t.Fatal("A is not connected to B after convergence")
	}
	nsB := nsOf(engB, 0x51F1)
	if !engB.connectedToNode(nsB, "A") {
		t.Fatal("B is not connected to A after convergence")
	}
}

// The ordering has to be the same on both ends or the tie is not broken, it is
// just moved. Whichever id sorts first is the initiator, so the survivor is the
// session that node initiated — and both sides agree which that is.
func TestSimultaneousOpenOrderingIsSymmetric(t *testing.T) {
	// Names chosen so the *second*-created engine sorts first, exercising the
	// branch where the local node abandons its own in-flight handshake.
	engA, engB, done := simOpenPair(t, 0x51F2, "zzz-second", "aaa-first")
	defer done()
	if !waitUntil(20*time.Second, func() bool {
		return sessionCount(engA) == 1 && sessionCount(engB) == 1
	}) {
		t.Fatalf("reversed ordering did not converge: A=%d B=%d", sessionCount(engA), sessionCount(engB))
	}
}
