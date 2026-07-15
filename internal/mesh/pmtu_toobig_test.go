package mesh

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// The scenario these cover, in one place:
//
// mcfed sat on a jumbo-frame LAN, so PMTU discovery had settled its peers at
// ~9000 bytes. It then roamed onto a cellular link with an MTU around 1400.
// Every packet still sized for the old link was refused by the kernel *before it
// left the host* — sendto returning EMSGSIZE ("message too long"). send() logged
// that at Debug and dropped the packet, then sent the same oversized packet again
// a second later, and again, indefinitely. The peer's eff MTU was never lowered,
// so nothing could get out; the node went completely dark and only recovered when
// the original jumbo link came back. Meanwhile the PMTU search sat waiting for
// probe *timeouts* to infer the very fact the kernel had already stated outright.

// TestPMTUTooBigFailsInFlightCandidateImmediately: EMSGSIZE on the probe we're
// currently awaiting must fail that candidate at once, rather than costing
// pmtuMaxTries × pmtuProbeTimeout to reach the same conclusion by timeout.
func TestPMTUTooBigFailsInFlightCandidateImmediately(t *testing.T) {
	now := time.Now()
	p := newPMTUState(1280, 9000, now)
	size, _, send := p.step(now, func() uint32 { return 1 })
	if !send {
		t.Fatal("expected an initial probe")
	}
	if !p.awaiting || p.cand != size {
		t.Fatalf("expected to be awaiting candidate %d", size)
	}

	if !p.tooBig(size, now) {
		t.Fatal("EMSGSIZE on the in-flight candidate must change state")
	}
	if p.awaiting {
		t.Error("the candidate must not still be awaiting an ack the kernel guaranteed will never come")
	}
	if p.high >= size {
		t.Errorf("upper bound must drop below the refused size: high=%d, refused=%d", p.high, size)
	}
}

// TestPMTUTooBigOnNonProbeDropsEffToFloor is the blackout case, and the one a
// probe timeout can never catch. If a keepalive or real traffic is refused, eff
// itself is too large — but there is no probe in flight to time out, so the
// search would never learn it and the peer stays blackholed forever. Falling
// back to the floor is always safe: it's the operator's configured known-good
// underlay MTU.
func TestPMTUTooBigOnNonProbeDropsEffToFloor(t *testing.T) {
	now := time.Now()
	p := newPMTUState(1280, 9000, now)
	// Pretend discovery already settled high, as it would on a jumbo LAN.
	p.eff, p.low, p.high, p.phase = 8900, 8900, 9000, phaseSettled

	// Now we roam to a ~1400-byte link and a normal 8900-byte packet is refused.
	if !p.tooBig(8900, now) {
		t.Fatal("EMSGSIZE on a packet at eff must change state")
	}
	if p.eff != 1280 {
		t.Fatalf("eff must fall back to the floor immediately, got %d — anything larger keeps being refused and the peer stays dark", p.eff)
	}
	if p.phase != phaseSearch {
		t.Errorf("must re-enter search after the path shrank, got phase %v", p.phase)
	}
}

// TestPMTUTooBigAtFloorIsInert: if even the floor is refused, the configured
// underlay_mtu is simply wrong for this path. There is nothing the state machine
// can do about that, and it must not thrash trying.
func TestPMTUTooBigAtFloorIsInert(t *testing.T) {
	now := time.Now()
	p := newPMTUState(1280, 9000, now)
	before := *p
	if p.tooBig(1280, now) {
		t.Fatal("EMSGSIZE at the floor must not change state — there is nowhere lower to go")
	}
	if p.eff != before.eff || p.phase != before.phase || p.high != before.high {
		t.Error("state must be left untouched when the floor itself is refused")
	}
}

// TestIsMsgTooLongUnwrapsTheRealError pins the detection. The transport hands
// back the error the way the net package builds it — a *net.OpError wrapping an
// *os.SyscallError wrapping the raw errno — which is exactly what the production
// log showed:
//
//	mesh: send: write udp4 0.0.0.0:65432->192.168.193.9:65432: sendto: message too long
//
// If this ever stops unwrapping, the clamp silently never fires and the blackout
// comes straight back, with nothing in the log to say why.
func TestIsMsgTooLongUnwrapsTheRealError(t *testing.T) {
	wrapped := &net.OpError{
		Op:  "write",
		Net: "udp4",
		Err: os.NewSyscallError("sendto", syscall.EMSGSIZE),
	}
	if !isMsgTooLong(wrapped) {
		t.Fatal("must recognise EMSGSIZE through the *net.OpError / *os.SyscallError chain the transport actually returns")
	}
	if isMsgTooLong(&net.OpError{Op: "write", Err: os.NewSyscallError("sendto", syscall.ENETUNREACH)}) {
		t.Error("ENETUNREACH is a routing failure, not an MTU verdict — it must not clamp the PMTU")
	}
	if isMsgTooLong(errors.New("some other failure")) {
		t.Error("an unrelated error must not clamp the PMTU")
	}
}
