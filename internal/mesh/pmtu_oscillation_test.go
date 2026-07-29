package mesh

import (
	"testing"
	"time"
)

// A mesh was observed stuck in a permanent path-MTU oscillation: every peer,
// continuously, climbing toward a 9000-byte ceiling on a ~1500-byte link,
// being refused at the same ladder of sizes, collapsing to the floor and
// climbing again — 648 refusals and 80 full resets in an 18-minute window,
// converging never.
//
// The cause was that EMSGSIZE lowered the search's upper *bound* but not its
// *ceiling*, and reset() restores high = ceil. So every reset discarded what
// the kernel had just taught and re-walked the identical range.
//
// EMSGSIZE is the local link refusing a datagram outright. That is a standing
// property of the path, and the whole point of these tests is that it has to
// outlive a reset.

func newTestPMTU(floor, ceil int) *pmtuState {
	p := &pmtuState{floor: floor, ceil: ceil}
	p.reset(time.Now())
	return p
}

func TestEMSGSIZELimitSurvivesReset(t *testing.T) {
	p := newTestPMTU(1280, 9000)
	now := time.Now()

	p.tooBig(1473, now) // the link refuses 1473

	if p.ceil >= 1473 {
		t.Fatalf("ceiling = %d after the link refused 1473, want below it", p.ceil)
	}
	p.reset(now)
	if p.high >= 1473 {
		t.Fatalf("after reset the search may climb to %d again, and will be refused at exactly the same size: this is the oscillation, one full cycle", p.high)
	}
	if p.ceil >= 1473 {
		t.Fatalf("reset restored ceiling to %d, discarding what EMSGSIZE established", p.ceil)
	}
}

// Repeated refusals must ratchet downward only.
func TestEMSGSIZELimitRatchets(t *testing.T) {
	p := newTestPMTU(1280, 9000)
	now := time.Now()
	for _, s := range []int{5212, 3342, 2407, 1939, 1705, 1588, 1530, 1501, 1473} {
		p.tooBig(s, now)
	}
	if p.ceil != 1472 {
		t.Fatalf("ceiling = %d after the observed ladder of refusals, want 1472 (one below the smallest)", p.ceil)
	}
	// A larger refusal afterwards must not raise it back.
	p.tooBig(5212, now)
	if p.ceil != 1472 {
		t.Fatalf("a later refusal at a larger size raised the ceiling to %d", p.ceil)
	}
}

// v703's relay cap restores the engine-wide ceiling on direct sessions. It must
// not undo a limit the link itself refused, or the oscillation returns by a
// different route every tick.
func TestRelayCapRestoreCannotRaiseTheLinkLimit(t *testing.T) {
	p := newTestPMTU(1280, 9000)
	p.tooBig(1473, time.Now())

	p.setCeil(9000) // what applyRelayMTUCap does for a session with no relay

	if p.ceil >= 1473 {
		t.Fatalf("restoring the engine-wide ceiling raised it to %d, past a size the link refuses", p.ceil)
	}
}

// The floor fallback is deliberate and must stay. A refusal at or below eff
// means the path changed under us, which makes low stale — it was confirmed
// against the old path, and on a jumbo LAN it can equal the size just refused.
// TestPMTUTooBigOnNonProbeDropsEffToFloor covers that directly; this asserts
// the property the oscillation fix must not break, since an earlier draft of
// it fell back to low and that test caught it.
func TestRefusalStillFallsToFloorButCeilingIsNowBounded(t *testing.T) {
	p := newTestPMTU(1280, 9000)
	now := time.Now()
	p.eff, p.low, p.high, p.phase = 8900, 8900, 9000, phaseSettled

	p.tooBig(8900, now)

	if p.eff != 1280 {
		t.Fatalf("eff = %d, want the floor: low was confirmed against the old path and is stale once the link refuses at or below eff", p.eff)
	}
	// The difference v709 makes: the re-climb is now bounded by what the link
	// refused, so it happens once instead of repeating forever.
	if p.ceil >= 8900 {
		t.Fatalf("ceiling = %d, want below 8900 — an unbounded re-climb is the oscillation", p.ceil)
	}
}
