package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestBuildICMPv4EchoRequestWellFormed(t *testing.T) {
	from := netip.MustParseAddr("10.0.0.1")
	to := netip.MustParseAddr("10.0.0.2")
	pkt := buildICMPv4EchoRequest(from, to, familyProbeICMPID, 7)

	if len(pkt) != 28 {
		t.Fatalf("length = %d, want 28 (20 IP + 8 ICMP)", len(pkt))
	}
	if pkt[0] != 0x45 {
		t.Errorf("version/IHL byte = %#x, want 0x45", pkt[0])
	}
	if pkt[9] != 1 {
		t.Errorf("protocol = %d, want 1 (ICMP)", pkt[9])
	}
	gotSrc, _ := netip.AddrFromSlice(pkt[12:16])
	if gotSrc != from {
		t.Errorf("src = %v, want %v", gotSrc, from)
	}
	gotDst, _ := netip.AddrFromSlice(pkt[16:20])
	if gotDst != to {
		t.Errorf("dst = %v, want %v", gotDst, to)
	}
	if pkt[20] != 8 {
		t.Errorf("ICMP type = %d, want 8 (echo request)", pkt[20])
	}
	// checksum16 over the whole IP header must be 0 once the checksum
	// field itself is included — the standard self-check property.
	if checksum16(pkt[:20]) != 0 {
		t.Error("IP header checksum does not self-verify")
	}
	// isICMPv4EchoReply must NOT match a request (type 8, not 0).
	if isICMPv4EchoReply(pkt) {
		t.Error("an echo request was misidentified as an echo reply")
	}
}

func TestBuildICMPv6EchoRequestWellFormed(t *testing.T) {
	from := netip.MustParseAddr("fd00::1")
	to := netip.MustParseAddr("fd00::2")
	pkt := buildICMPv6EchoRequest(from, to, familyProbeICMPID, 3)

	if len(pkt) != 48 {
		t.Fatalf("length = %d, want 48 (40 IP + 8 ICMPv6)", len(pkt))
	}
	if pkt[0]>>4 != 6 {
		t.Errorf("version nibble = %d, want 6", pkt[0]>>4)
	}
	if pkt[6] != 58 {
		t.Errorf("next header = %d, want 58 (ICMPv6)", pkt[6])
	}
	gotSrc, _ := netip.AddrFromSlice(pkt[8:24])
	if gotSrc != from {
		t.Errorf("src = %v, want %v", gotSrc, from)
	}
	gotDst, _ := netip.AddrFromSlice(pkt[24:40])
	if gotDst != to {
		t.Errorf("dst = %v, want %v", gotDst, to)
	}
	if pkt[40] != 128 {
		t.Errorf("ICMPv6 type = %d, want 128 (echo request)", pkt[40])
	}
	if isICMPv6EchoReply(pkt) {
		t.Error("an echo request was misidentified as an echo reply")
	}
}

// flipToEchoReply mutates a copy of an echo *request* packet (as built by
// buildICMPv4EchoRequest/buildICMPv6EchoRequest) into the corresponding
// reply — swapping src/dst and changing the type byte — the same
// transformation a real OS kernel performs automatically when answering a
// ping. Used by both the parser tests below and the end-to-end round-trip
// test to simulate "the peer's OS replied" without needing a real TUN.
func flipToEchoReply(req []byte) []byte {
	out := append([]byte(nil), req...)
	if out[0]>>4 == 4 {
		for i := 0; i < 4; i++ {
			out[12+i], out[16+i] = out[16+i], out[12+i]
		}
		out[20] = 0 // echo reply
		out[22], out[23] = 0, 0
		sum4 := checksum16(out[20:28])
		out[22] = byte(sum4 >> 8)
		out[23] = byte(sum4)
		return out
	}
	var tmp [16]byte
	copy(tmp[:], out[8:24])
	copy(out[8:24], out[24:40])
	copy(out[24:40], tmp[:])
	out[40] = 129 // echo reply
	out[42], out[43] = 0, 0
	src, _ := netip.AddrFromSlice(out[8:24])
	dst, _ := netip.AddrFromSlice(out[24:40])
	sum := icmpv6Checksum(src, dst, out[40:48])
	out[42] = byte(sum >> 8)
	out[43] = byte(sum)
	return out
}

func TestFlippedRequestIsRecognizedAsReply(t *testing.T) {
	req4 := buildICMPv4EchoRequest(netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), familyProbeICMPID, 1)
	rep4 := flipToEchoReply(req4)
	if !isICMPv4EchoReply(rep4) {
		t.Error("flipped v4 request not recognized as an echo reply")
	}
	if !srcIs(rep4, netip.MustParseAddr("10.0.0.2")) {
		t.Error("flipped v4 reply's source should now be the original dst")
	}

	req6 := buildICMPv6EchoRequest(netip.MustParseAddr("fd00::1"), netip.MustParseAddr("fd00::2"), familyProbeICMPID, 1)
	rep6 := flipToEchoReply(req6)
	if !isICMPv6EchoReply(rep6) {
		t.Error("flipped v6 request not recognized as an echo reply")
	}
	if !srcIs(rep6, netip.MustParseAddr("fd00::2")) {
		t.Error("flipped v6 reply's source should now be the original dst")
	}
}

func TestIsICMPEchoReplyRejectsNonReplies(t *testing.T) {
	// A well-formed but too-short buffer must fail closed, not panic.
	if isICMPv4EchoReply([]byte{0x45, 0, 0}) {
		t.Error("truncated v4 buffer should not be recognized as a reply")
	}
	if isICMPv6EchoReply([]byte{0x60, 0, 0}) {
		t.Error("truncated v6 buffer should not be recognized as a reply")
	}
	// A v4 UDP packet (protocol 17, not 1) must not be mistaken for ICMP.
	udp := make([]byte, 28)
	udp[0] = 0x45
	udp[9] = 17
	if isICMPv4EchoReply(udp) {
		t.Error("a UDP packet was misidentified as an ICMP echo reply")
	}
}

func TestFamilyLiveOptimisticBeforeFirstProbe(t *testing.T) {
	now := time.Now()
	if !familyLive(0, 0, now) {
		t.Error("a family that's never been probed at all should read live (optimistic default)")
	}
}

func TestFamilyLiveGracePeriodBeforeFirstReply(t *testing.T) {
	now := time.Now()
	justStarted := now.Add(-1 * time.Second).UnixNano()
	if !familyLive(justStarted, 0, now) {
		t.Error("a family probed only 1s ago with no reply yet should still read live (grace window)")
	}
}

func TestFamilyLiveDeadAfterGraceWithNoReply(t *testing.T) {
	now := time.Now()
	longAgo := now.Add(-(familyDeadAfter + time.Second)).UnixNano()
	if familyLive(longAgo, 0, now) {
		t.Error("a family probed well past familyDeadAfter with zero replies should read dead")
	}
}

func TestFamilyLiveRecentReply(t *testing.T) {
	now := time.Now()
	longAgo := now.Add(-1 * time.Hour).UnixNano() // first probe ages ago
	recentGood := now.Add(-1 * time.Second).UnixNano()
	if !familyLive(longAgo, recentGood, now) {
		t.Error("a family with a reply 1s ago should read live regardless of how long ago probing started")
	}
}

func TestFamilyLiveStaleReplyPastDeadAfter(t *testing.T) {
	now := time.Now()
	longAgo := now.Add(-1 * time.Hour).UnixNano()
	staleGood := now.Add(-(familyDeadAfter + time.Second)).UnixNano()
	if familyLive(longAgo, staleGood, now) {
		t.Error("a family whose last good reply is older than familyDeadAfter should read dead")
	}
}
