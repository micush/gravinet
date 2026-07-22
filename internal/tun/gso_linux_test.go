//go:build linux && (amd64 || arm64)

package tun

import (
	"encoding/binary"
	"testing"
)

// buildTCPv4 builds a minimal, fully valid IPv4/TCP packet (no options) with
// the given fields and payload, checksums included. Used throughout as the
// "known good packet" fixture every test starts from.
func buildTCPv4(t *testing.T, srcIP, dstIP [4]byte, sport, dport uint16, seq, ack uint32, flags byte, payload []byte) []byte {
	t.Helper()
	pkt := make([]byte, gsoHdrLen+len(payload))
	pkt[0] = 0x45 // version 4, IHL 5
	pkt[1] = 0    // DSCP/ECN
	binary.BigEndian.PutUint16(pkt[ipTotalLenOff:], uint16(len(pkt)))
	binary.BigEndian.PutUint16(pkt[ipIDOff:], 0x1234)
	binary.BigEndian.PutUint16(pkt[ipFlagsFragOff:], 0x4000) // DF
	pkt[ipTTLOff] = 64
	pkt[ipProtoOff] = 6 // TCP
	copy(pkt[ipSrcOff:], srcIP[:])
	copy(pkt[ipDstOff:], dstIP[:])

	tcp := pkt[ipv4HeaderLen:]
	binary.BigEndian.PutUint16(tcp[tcpSrcPortOff:], sport)
	binary.BigEndian.PutUint16(tcp[tcpDstPortOff:], dport)
	binary.BigEndian.PutUint32(tcp[tcpSeqOff:], seq)
	binary.BigEndian.PutUint32(tcp[tcpAckOff:], ack)
	tcp[12] = 5 << 4 // data offset 5, no options
	tcp[tcpFlagsOff] = flags
	binary.BigEndian.PutUint16(tcp[tcpWindowOff:], 65535)
	copy(tcp[20:], payload)

	binary.BigEndian.PutUint16(pkt[ipChecksumOff:], ipv4Checksum(pkt[:ipv4HeaderLen]))
	binary.BigEndian.PutUint16(tcp[tcpChecksumOff:], tcpChecksum(pkt[:ipv4HeaderLen], tcp))
	return pkt
}

// verifyChecksumsValid re-derives both checksums the way a receiver would —
// summing everything, including the checksum field as sent — and requires
// the RFC 1071 self-check property: a correct checksum makes the total sum
// fold to 0xffff.
func verifyChecksumsValid(t *testing.T, pkt []byte) {
	t.Helper()
	var ipsum uint32
	for i := 0; i < ipv4HeaderLen; i += 2 {
		ipsum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	for ipsum>>16 != 0 {
		ipsum = (ipsum & 0xffff) + (ipsum >> 16)
	}
	if ipsum != 0xffff {
		t.Errorf("IPv4 header checksum self-check failed: folded sum = %#x, want 0xffff", ipsum)
	}

	tcpSeg := pkt[ipv4HeaderLen:]
	var sum uint32
	sum += uint32(binary.BigEndian.Uint16(pkt[ipSrcOff : ipSrcOff+2]))
	sum += uint32(binary.BigEndian.Uint16(pkt[ipSrcOff+2 : ipSrcOff+4]))
	sum += uint32(binary.BigEndian.Uint16(pkt[ipDstOff : ipDstOff+2]))
	sum += uint32(binary.BigEndian.Uint16(pkt[ipDstOff+2 : ipDstOff+4]))
	sum += uint32(pkt[ipProtoOff])
	sum += uint32(len(tcpSeg))
	sum += sumBytes(tcpSeg)
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	if sum != 0xffff {
		t.Errorf("TCP checksum self-check failed: folded sum = %#x, want 0xffff", sum)
	}
}

func TestBuildTCPv4ChecksumsValid(t *testing.T) {
	pkt := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 1234, 443, 1000, 2000, tcpFlagACK, []byte("hello world"))
	verifyChecksumsValid(t, pkt)
}

func TestBuildTCPv4ChecksumsValidOddPayload(t *testing.T) {
	// sumBytes' odd-trailing-byte handling only actually runs when the TCP
	// segment (header+payload) has an odd total length; header is always
	// 20, so this needs an odd-length payload specifically.
	pkt := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 1, 2, 0, 0, tcpFlagACK, []byte("odd"))
	verifyChecksumsValid(t, pkt)
}

func TestIsPlainIPv4TCP(t *testing.T) {
	good := buildTCPv4(t, [4]byte{1, 1, 1, 1}, [4]byte{2, 2, 2, 2}, 1, 2, 0, 0, tcpFlagACK, []byte("x"))
	if !isPlainIPv4TCP(good) {
		t.Error("well-formed packet rejected")
	}
	if isPlainIPv4TCP(good[:10]) {
		t.Error("truncated packet accepted")
	}
	withOpts := append([]byte(nil), good...)
	withOpts[0] = 0x46 // IHL 6: IP options present
	if isPlainIPv4TCP(withOpts) {
		t.Error("packet with IP options accepted")
	}
	notTCP := append([]byte(nil), good...)
	notTCP[ipProtoOff] = 17 // UDP
	if isPlainIPv4TCP(notTCP) {
		t.Error("non-TCP packet accepted")
	}
}

// gsoSuperPacket builds a synthetic coalesced buffer the way a real kernel
// GSO read would hand it to ReadPackets: one IPv4/TCP header sized for the
// *first* segment, followed by nSeg*segSize bytes of payload (the last
// chunk possibly short), with a vnetHdr describing it — exactly the input
// shape splitGSO is contracted to accept.
func gsoSuperPacket(t *testing.T, seq uint32, segSize uint16, totalPayload int) ([]byte, vnetHdr) {
	t.Helper()
	payload := make([]byte, totalPayload)
	for i := range payload {
		payload[i] = byte(i)
	}
	pkt := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 5000, 443, seq, 1, tcpFlagACK|tcpFlagPSH, payload)
	h := vnetHdr{gsoType: gsoTCPv4, hdrLen: gsoHdrLen, gsoSize: segSize}
	return pkt, h
}

func TestSplitGSORoundTrip(t *testing.T) {
	const segSize = 1400
	const totalPayload = segSize*3 + 137 // 3 full segments + 1 short one
	startSeq := uint32(5_000_000)
	pkt, h := gsoSuperPacket(t, startSeq, segSize, totalPayload)

	var segs [][]byte
	scratch := make([]byte, gsoHdrLen+segSize)
	ok := splitGSO(h, pkt, scratch, func(seg []byte) {
		cp := make([]byte, len(seg))
		copy(cp, seg)
		segs = append(segs, cp)
	})
	if !ok {
		t.Fatal("splitGSO reported false for a valid super-packet")
	}
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4 (3 full + 1 short)", len(segs))
	}

	var recombined []byte
	seq := startSeq
	for i, seg := range segs {
		if !isPlainIPv4TCP(seg) {
			t.Fatalf("segment %d is not a well-formed plain TCPv4 packet", i)
		}
		verifyChecksumsValid(t, seg)
		if got := tcpSeq(seg); got != seq {
			t.Errorf("segment %d seq = %d, want %d", i, got, seq)
		}
		payload := seg[gsoHdrLen:]
		wantLen := segSize
		if i == len(segs)-1 {
			wantLen = totalPayload - segSize*(len(segs)-1)
		}
		if len(payload) != wantLen {
			t.Errorf("segment %d payload len = %d, want %d", i, len(payload), wantLen)
		}
		gotTotal := ipTotalLen(seg)
		if int(gotTotal) != len(seg) {
			t.Errorf("segment %d IP total length field = %d, want %d (actual buffer size)", i, gotTotal, len(seg))
		}
		flags := tcpFlags(seg)
		if i != len(segs)-1 {
			if flags&(tcpFlagFIN|tcpFlagPSH) != 0 {
				t.Errorf("segment %d (not last) kept FIN/PSH: flags=%#x", i, flags)
			}
		} else if flags&tcpFlagPSH == 0 {
			t.Errorf("last segment lost PSH: flags=%#x", flags)
		}
		seq += uint32(len(payload))
		recombined = append(recombined, payload...)
	}

	origPayload := pkt[gsoHdrLen:]
	if len(recombined) != len(origPayload) {
		t.Fatalf("recombined payload len = %d, want %d", len(recombined), len(origPayload))
	}
	for i := range origPayload {
		if recombined[i] != origPayload[i] {
			t.Fatalf("recombined payload differs from original at byte %d", i)
			break
		}
	}
}

func TestSplitGSORejectsNonGSO(t *testing.T) {
	pkt, _ := gsoSuperPacket(t, 0, 1400, 1400) // exactly one segment's worth
	h := vnetHdr{gsoType: gsoNone}
	called := false
	ok := splitGSO(h, pkt, make([]byte, 2000), func([]byte) { called = true })
	if ok || called {
		t.Error("splitGSO should refuse a plain (non-GSO) header")
	}

	h2 := vnetHdr{gsoType: gsoTCPv4, gsoSize: 1400}
	ok2 := splitGSO(h2, pkt, make([]byte, 2000), func([]byte) { called = true })
	if ok2 || called {
		t.Error("splitGSO should refuse a super-packet that is only one segment long")
	}
}

func TestSplitGSORejectsSmallScratch(t *testing.T) {
	pkt, h := gsoSuperPacket(t, 0, 1400, 1400*3)
	ok := splitGSO(h, pkt, make([]byte, 10), func([]byte) {
		t.Fatal("emit called despite undersized scratch")
	})
	if ok {
		t.Error("splitGSO should refuse when scratch is too small, not overrun it")
	}
}

// contiguousSegments builds n packets of a single TCP flow, each carrying
// segSize bytes (the last one short by shortBy), consecutive seq numbers —
// exactly a run TryAdd should accept end to end.
func contiguousSegments(t *testing.T, n int, segSize uint16, shortBy int) [][]byte {
	t.Helper()
	segs := make([][]byte, n)
	seq := uint32(9_000_000)
	for i := 0; i < n; i++ {
		size := int(segSize)
		if i == n-1 {
			size -= shortBy
		}
		payload := make([]byte, size)
		for j := range payload {
			payload[j] = byte(i*31 + j)
		}
		flags := byte(tcpFlagACK)
		if i == n-1 {
			flags |= tcpFlagPSH
		}
		segs[i] = buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6000, 443, seq, 1, flags, payload)
		seq += uint32(size)
	}
	return segs
}

func TestCoalescerRoundTripThroughSplit(t *testing.T) {
	segs := contiguousSegments(t, 4, 1400, 200) // 3 full + 1 short(1200)
	c := NewCoalescer()
	for i, seg := range segs {
		if !c.TryAdd(seg) {
			t.Fatalf("TryAdd rejected contiguous segment %d", i)
		}
	}
	merged, segSize, n := c.Flush()
	if n != 4 {
		t.Fatalf("Flush reported %d segments merged, want 4", n)
	}
	if segSize != 1400 {
		t.Fatalf("Flush segSize = %d, want 1400", segSize)
	}
	verifyChecksumsValid(t, merged)

	// Feed the merged buffer straight back into splitGSO — this is the
	// real end-to-end guarantee: whatever the Coalescer built on the write
	// side must be exactly what splitGSO can take apart on the read side,
	// since a forwarding hop's kernel will do precisely this with it.
	h := vnetHdr{gsoType: gsoTCPv4, hdrLen: gsoHdrLen, gsoSize: segSize}
	var resplit [][]byte
	scratch := make([]byte, gsoHdrLen+int(segSize))
	if !splitGSO(h, merged, scratch, func(seg []byte) {
		cp := append([]byte(nil), seg...)
		resplit = append(resplit, cp)
	}) {
		t.Fatal("splitGSO refused the Coalescer's own merged output")
	}
	if len(resplit) != len(segs) {
		t.Fatalf("re-split into %d segments, want %d", len(resplit), len(segs))
	}
	for i := range segs {
		wantPayload := segs[i][gsoHdrLen:]
		gotPayload := resplit[i][gsoHdrLen:]
		if len(gotPayload) != len(wantPayload) {
			t.Fatalf("segment %d re-split payload len = %d, want %d", i, len(gotPayload), len(wantPayload))
		}
		for j := range wantPayload {
			if gotPayload[j] != wantPayload[j] {
				t.Fatalf("segment %d payload differs from original at byte %d", i, j)
			}
		}
		verifyChecksumsValid(t, resplit[i])
	}
}

func TestCoalescerSingleSegmentPassesThroughUnmodified(t *testing.T) {
	segs := contiguousSegments(t, 1, 1400, 0)
	c := NewCoalescer()
	if !c.TryAdd(segs[0]) {
		t.Fatal("TryAdd rejected the only segment offered")
	}
	buf, _, n := c.Flush()
	if n != 1 {
		t.Fatalf("Flush reported %d segments, want 1", n)
	}
	if len(buf) != len(segs[0]) {
		t.Fatalf("single-segment Flush changed length: got %d, want %d", len(buf), len(segs[0]))
	}
	for i := range buf {
		if buf[i] != segs[0][i] {
			t.Fatalf("single-segment Flush modified byte %d: got %#x, want %#x", i, buf[i], segs[0][i])
		}
	}
}

func TestCoalescerFlushEmpty(t *testing.T) {
	c := NewCoalescer()
	buf, segSize, n := c.Flush()
	if n != 0 || buf != nil || segSize != 0 {
		t.Fatalf("Flush on an empty Coalescer = (%v, %d, %d), want (nil, 0, 0)", buf, segSize, n)
	}
}

func TestCoalescerRejectsDifferentFlow(t *testing.T) {
	a := contiguousSegments(t, 1, 1400, 0)[0]
	b := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6001 /* different sport */, 443, 9_001_400, 1, tcpFlagACK, make([]byte, 1400))
	c := NewCoalescer()
	if !c.TryAdd(a) {
		t.Fatal("TryAdd rejected the first, run-starting segment")
	}
	if c.TryAdd(b) {
		t.Error("TryAdd accepted a segment from a different flow into an in-progress run")
	}
}

func TestCoalescerRejectsNonContiguousSeq(t *testing.T) {
	segs := contiguousSegments(t, 2, 1400, 0)
	// Corrupt the second segment's sequence number so it no longer follows
	// the first.
	binary.BigEndian.PutUint32(segs[1][ipv4HeaderLen+tcpSeqOff:], tcpSeq(segs[1])+1)
	c := NewCoalescer()
	if !c.TryAdd(segs[0]) {
		t.Fatal("TryAdd rejected the first segment")
	}
	if c.TryAdd(segs[1]) {
		t.Error("TryAdd accepted a non-contiguous sequence number")
	}
}

func TestCoalescerRejectsAfterShortSegment(t *testing.T) {
	// A run where the *middle* segment is short must refuse anything after
	// it — a short segment may only ever be the last one in a valid GSO
	// buffer.
	segs := contiguousSegments(t, 3, 1400, 0)
	// Shrink segment 1's payload without fixing up segment 2's seq to
	// match, so segment 2 is simultaneously "the wrong seq" AND "offered
	// after a short mid-run segment" — trim segment 1 and recompute
	// checksums so it's still a well-formed (if short) packet, but leave
	// its own seq/len internally consistent.
	shortPayload := segs[1][gsoHdrLen : gsoHdrLen+900]
	shortSeg := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6000, 443, tcpSeq(segs[1]), 1, tcpFlagACK, shortPayload)

	c := NewCoalescer()
	if !c.TryAdd(segs[0]) {
		t.Fatal("TryAdd rejected segment 0")
	}
	if !c.TryAdd(shortSeg) {
		t.Fatal("TryAdd rejected the short (but contiguous) segment 1")
	}
	// segs[2] has the seq that would have followed a *full* segment 1, not
	// the short one just added, so this also exercises the ordinary
	// non-contiguous-seq rejection — construct a version with the seq
	// that *would* be contiguous after the short segment, to isolate the
	// "short segment must be last" rule specifically.
	nextSeq := tcpSeq(shortSeg) + uint32(len(shortPayload))
	next := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6000, 443, nextSeq, 1, tcpFlagACK, make([]byte, 1400))
	if c.TryAdd(next) {
		t.Error("TryAdd accepted a segment following a short (non-final) segment")
	}
}

func TestCoalescerRejectsSYNRST(t *testing.T) {
	base := contiguousSegments(t, 1, 1400, 0)[0]
	for _, flag := range []byte{tcpFlagSYN, tcpFlagRST} {
		pkt := append([]byte(nil), base...)
		pkt[ipv4HeaderLen+tcpFlagsOff] |= flag
		c := NewCoalescer()
		if c.TryAdd(pkt) {
			t.Errorf("TryAdd accepted a segment with flag %#x set", flag)
		}
	}
}

func TestCoalescerRejectsBareAck(t *testing.T) {
	pkt := buildTCPv4(t, [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, 6000, 443, 9_000_000, 1, tcpFlagACK, nil)
	c := NewCoalescer()
	if c.TryAdd(pkt) {
		t.Error("TryAdd accepted a zero-payload (bare ack) segment")
	}
}

func TestVnetHdrRoundTrip(t *testing.T) {
	h := vnetHdr{flags: vnetHdrFlagNeedsCSUM, gsoType: gsoTCPv4, hdrLen: 40, gsoSize: 1400, csumStart: 34, csumOff: 16}
	buf := make([]byte, vnetHdrLen)
	h.put(buf)
	got := getVnetHdr(buf)
	if got != h {
		t.Errorf("vnetHdr round trip: got %+v, want %+v", got, h)
	}
}
