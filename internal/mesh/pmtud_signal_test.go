package mesh

import (
	"encoding/binary"
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// mkV4 builds a minimal IPv4 packet of total length n. df sets the Don't
// Fragment bit; proto 6 (TCP) unless overridden.
func mkV4(src, dst netip.Addr, n int, df bool, proto byte) []byte {
	p := make([]byte, n)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(n))
	if df {
		p[6] = 0x40
	}
	p[8] = 64
	p[9] = proto
	a := src.As4()
	b := dst.As4()
	copy(p[12:16], a[:])
	copy(p[16:20], b[:])
	return p
}

// sendDataRecovering calls sendData on a fixture that has no session crypto.
// The advisory is written to the device before the packet is split, and the
// split then panics inside Seal; the write under test has already happened by
// then, so recovering leaves exactly what is being asserted on. Building real
// session crypto here would test the cipher, not the advisory.
func sendDataRecovering(e *Engine, ps *peerSession, pkt []byte) {
	defer func() { _ = recover() }()
	e.sendData(ps, pkt)
}

func newSignalFixture(t *testing.T) (*Engine, *netState, *peerSession, *fakeDev) {
	t.Helper()
	dev := newFakeDev("mesh0")
	ns := &netState{self4: netip.MustParseAddr("192.168.203.5")}
	ns.spec.Dev = dev
	ps := &peerSession{nodeID: "mcfed", net: ns}
	ps.setEff(1473) // the real mcfed figure: jumbo LAN one side, ~1500 the other
	e := &Engine{nodeID: "gn-cush2", log: logx.New(io.Discard, logx.LevelDebug)}
	return e, ns, ps, dev
}

// A packet too large for the peer's path must still be forwarded — the
// advisory is additive, and a sender that ignores ICMP must be no worse off
// than before this existed.
func TestOversizedPacketStillFragmentedAndAdvised(t *testing.T) {
	e, _, ps, dev := newSignalFixture(t)
	src := netip.MustParseAddr("192.168.5.116")
	dst := netip.MustParseAddr("192.168.203.111")

	per := int(ps.maxFrag.Load())
	if per <= 0 {
		t.Fatal("fixture produced no fragment size")
	}
	sendDataRecovering(e, ps, mkV4(src, dst, per+2000, true, 6))

	select {
	case out := <-dev.out:
		if out[0]>>4 != 4 || out[9] != 1 {
			t.Fatalf("advisory is not an IPv4 ICMP packet: version=%d proto=%d", out[0]>>4, out[9])
		}
		m := out[20:]
		if m[0] != 3 || m[1] != 4 {
			t.Fatalf("ICMP type/code = %d/%d, want 3/4 (fragmentation needed)", m[0], m[1])
		}
		if got := int(binary.BigEndian.Uint16(m[6:8])); got != per {
			t.Fatalf("advertised next-hop MTU = %d, want %d — a wrong number here is worse than none, the sender will act on it", got, per)
		}
		// Addressed back to the sender, from us.
		if got, _ := netip.AddrFromSlice(out[16:20]); got != src {
			t.Fatalf("advisory destination = %v, want the original sender %v", got, src)
		}
	case <-time.After(time.Second):
		t.Fatal("no path-MTU advisory written to the overlay device: the sender will keep emitting oversized packets indefinitely, and every one costs a multi-fragment split where losing any single datagram discards the whole packet")
	}
}

// Without DF the sender has explicitly permitted fragmentation; RFC 1191
// signalling would be unsolicited and misleading.
func TestNoAdvisoryWithoutDontFragment(t *testing.T) {
	e, _, ps, dev := newSignalFixture(t)
	per := int(ps.maxFrag.Load())
	sendDataRecovering(e, ps, mkV4(netip.MustParseAddr("192.168.5.116"), netip.MustParseAddr("192.168.203.111"), per+2000, false, 6))

	select {
	case out := <-dev.out:
		t.Fatalf("advised a sender that did not set DF (%d bytes written)", len(out))
	case <-time.After(150 * time.Millisecond):
	}
}

// Replying to an ICMP error with another ICMP error is how storms start.
func TestNoAdvisoryInReplyToICMPError(t *testing.T) {
	e, _, ps, dev := newSignalFixture(t)
	per := int(ps.maxFrag.Load())
	pkt := mkV4(netip.MustParseAddr("192.168.5.116"), netip.MustParseAddr("192.168.203.111"), per+2000, true, 1)
	pkt[20] = 3 // ICMP destination unreachable

	sendDataRecovering(e, ps, pkt)
	select {
	case <-dev.out:
		t.Fatal("advised in reply to an ICMP error message")
	case <-time.After(150 * time.Millisecond):
	}
}

// The trigger is per-packet; a bulk transfer must not produce an advisory per
// datagram.
func TestAdvisoryIsRateLimitedPerSession(t *testing.T) {
	e, _, ps, dev := newSignalFixture(t)
	src := netip.MustParseAddr("192.168.5.116")
	dst := netip.MustParseAddr("192.168.203.111")
	per := int(ps.maxFrag.Load())

	for i := 0; i < 25; i++ {
		sendDataRecovering(e, ps, mkV4(src, dst, per+2000, true, 6))
	}
	n := 0
	for {
		select {
		case <-dev.out:
			n++
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if n != 1 {
		t.Fatalf("25 oversized packets produced %d advisories, want 1: the limiter is what keeps a bulk transfer from generating one ICMP per datagram", n)
	}
	if got := ps.tooBigSent.Load(); got != 1 {
		t.Fatalf("tooBigSent = %d, want 1", got)
	}
}

// A packet that fits must produce nothing at all.
func TestNoAdvisoryForPacketThatFits(t *testing.T) {
	e, _, ps, dev := newSignalFixture(t)
	sendDataRecovering(e, ps, mkV4(netip.MustParseAddr("192.168.5.116"), netip.MustParseAddr("192.168.203.111"), 200, true, 6))
	select {
	case <-dev.out:
		t.Fatal("advised on a packet that needed no fragmentation")
	case <-time.After(150 * time.Millisecond):
	}
}
