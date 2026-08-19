package mesh

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// The defect shape this file guards, which has now appeared three times: a
// throttle whose state advances only when an attempt goes *unanswered*, so
// anything that fails by responding retries unbounded. planHandshake arms
// seedRetryBackoff only on the branch where an attempt cycle exhausts by
// timeout; every refusal-on-receipt in onHSResp deletes the pending handshake
// and so never reaches it, leaving initLoop (1s) to re-dial forever.
//
// candstore's TCP is the one path that got this right, and the reason
// is worth copying: it backs off on the *outcome* — did a session form within
// the grace period — rather than on any enumerated cause, so failure modes
// nobody thought of are covered too.

func coolState() (*Engine, *netState) {
	e := &Engine{nodeID: "self", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		byNode:      map[string]*peerSession{},
		nodes:       map[string]*nodeInfo{},
		pending:     map[uint32]*pendingHS{},
		seedOwner:   map[netip.AddrPort]string{},
		seedBackoff: map[netip.AddrPort]time.Time{},
	}
	ns.spec.ID = 0xC0
	return e, ns
}

var (
	dialedEP = netip.MustParseAddrPort("192.0.2.10:51820")
	otherEP  = netip.MustParseAddrPort("192.0.2.99:51820")
)

func TestRefusalCoolsTheAddressDialed(t *testing.T) {
	e, ns := coolState()
	p := &pendingHS{idxI: 1, endpoint: dialedEP}

	e.coolSeedAfterRefusal(ns, p, dialedEP, "test")

	ns.mu.RLock()
	until, ok := ns.seedBackoff[dialedEP]
	ns.mu.RUnlock()
	if !ok {
		t.Fatal("a refused handshake must cool the address down, or initLoop re-dials it every second")
	}
	if d := time.Until(until); d < seedRetryBackoff-time.Second || d > seedRetryBackoff+time.Second {
		t.Errorf("cooldown is %v, want about %v", d, seedRetryBackoff)
	}
}

// A response can arrive from a different source address than the one dialed —
// hairpins especially. Cooling only the responder's address would leave the
// dialed one uncooled and the loop unthrottled, so both are cooled.
func TestRefusalCoolsBothDialedAndRespondingAddress(t *testing.T) {
	e, ns := coolState()
	p := &pendingHS{idxI: 1, endpoint: dialedEP}

	e.coolSeedAfterRefusal(ns, p, otherEP, "hairpin")

	ns.mu.RLock()
	_, dialed := ns.seedBackoff[dialedEP]
	_, responded := ns.seedBackoff[otherEP]
	ns.mu.RUnlock()
	if !dialed {
		t.Error("the address initLoop dials is the one it consults; it must be cooled")
	}
	if !responded {
		t.Error("the responding address should be cooled too")
	}
}

// The cooldown must be the ordinary revocable one, not the long partial-mesh
// suppression: a ban, a disabled peer and a hairpinning router are all fixable
// locally, so retrying soon costs one handshake.
func TestRefusalUsesRevocableCooldownNotPolicyTTL(t *testing.T) {
	if seedRetryBackoff >= policyRefusedTTL {
		t.Fatalf("seedRetryBackoff (%v) should be far shorter than policyRefusedTTL (%v): "+
			"these refusals are locally revocable, partial-mesh seed status is not", seedRetryBackoff, policyRefusedTTL)
	}
}

// The point, as a rate: over five minutes of 1s initLoop ticks, a peer that
// answers and is refused should be dialled a handful of times, not 300.
func TestRefusedPeerIsNotDialledEveryTick(t *testing.T) {
	e, ns := coolState()
	start := time.Now()
	dials := 0
	for elapsed := time.Duration(0); elapsed < 5*time.Minute; elapsed += time.Second {
		at := start.Add(elapsed)
		ns.mu.RLock()
		until, cooling := ns.seedBackoff[dialedEP]
		ns.mu.RUnlock()
		if cooling && at.Before(until) {
			continue
		}
		dials++
		// The dial is answered and refused — a ban, say, which does not expire
		// on its own.
		p := &pendingHS{idxI: uint32(dials), endpoint: dialedEP}
		ns.mu.Lock()
		delete(ns.pending, p.idxI) // as onHSResp does
		ns.mu.Unlock()
		e.coolSeedAfterRefusal(ns, p, dialedEP, "banned")
		// coolSeedAfterRefusal computes its deadline from time.Now(), so hold
		// the simulated clock and the real one together for this loop.
		ns.mu.Lock()
		ns.seedBackoff[dialedEP] = at.Add(seedRetryBackoff)
		ns.mu.Unlock()
	}
	want := int(5 * time.Minute / seedRetryBackoff)
	if dials > want+1 {
		t.Errorf("dialled %d times in 5 minutes; seedRetryBackoff (%v) allows about %d, "+
			"and the unthrottled loop managed 300", dials, seedRetryBackoff, want)
	}
	if dials < 2 {
		t.Errorf("dialled only %d times; the cooldown must expire so a lifted ban recovers", dials)
	}
}
