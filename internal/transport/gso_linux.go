//go:build linux && (amd64 || arm64)

// This file is gravinet's Phase B: UDP GSO (UDP_SEGMENT) on send and UDP GRO
// (UDP_GRO) on receive, layered onto the Phase A batching in batch_linux.go.
// Phase A amortised the *syscall boundary* — one sendmmsg/recvmmsg per batch —
// but the kernel still runs its full per-datagram path (route lookup, header
// build, qdisc, skb alloc) for every message inside that call. UDP_SEGMENT
// hands the kernel ONE buffer holding several equal-size datagrams to the same
// destination plus a stride, and the kernel (or the NIC, with hardware
// tx-udp-segmentation) splits it late, amortising the whole stack traversal.
// UDP_GRO is the mirror: the kernel coalesces consecutive same-flow datagrams
// and delivers one buffer with the stride in a cmsg; the reader slices it back
// apart. This is the optimisation that took wireguard-go from ~1.4 Gbps to
// 7-13 Gbps in Tailscale's published work.
//
// Why this is a fundamentally different risk class from Phase C (v572-v575,
// attempted, field-broken twice, reverted): nothing here rewrites packet
// bytes. Every segment is an independent, already-sealed gravinet datagram;
// TX "coalescing" is concatenation-by-iovec plus one stride integer, and RX
// "splitting" is slicing at offsets the kernel reports. There is no checksum
// arithmetic, no TCP header surgery, no sequence-number math — the entire bug
// class that killed Phase C does not exist on this path. And the known failure
// modes are LOUD: a kernel or driver that can't do a GSO send returns an errno
// (EIO/EINVAL/EOPNOTSUPP) from sendmsg rather than silently corrupting bytes;
// sendGSORun (batch_linux.go) treats any such error by permanently disabling
// GSO for that socket and re-sending the run per-packet — safe against
// duplication because a failed sendmsg is atomic: nothing reached the wire.
//
// DEFAULT OFF. GRAVINET_UDP_GSO=1 enables it; anything else leaves v571
// behaviour exactly. The Phase C arc's closing lesson (see v575 in
// docs/changelog.md) is binding here: loopback-verifiable correctness — which
// this file HAS, unlike what v556's entry assumed; UDP GSO/GRO gained a
// kernel software fallback that loopback exercises — is necessary but NOT
// sufficient. What loopback cannot exercise is real NIC offload engagement
// and driver-specific failure behaviour, which is the entire reason the
// disable-on-error fallback exists. The bar for defaulting this on is a real
// two-machine rig on the target link, ethtool-confirmed offload, sustained
// bulk TCP AND interactive SSH through the mesh. Not before.

package transport

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

const (
	// solUDP/udpSegment/udpGRO are include/uapi/linux/udp.h's SOL_UDP,
	// UDP_SEGMENT, and UDP_GRO. Spelled out locally like sysSendmmsg
	// (batchnr_linux_*.go): the stdlib syscall package predates them.
	solUDP     = 17
	udpSegment = 103
	udpGRO     = 104

	// maxGSOSegs caps how many datagrams one UDP_SEGMENT send may carry.
	// The kernel's own ceiling (UDP_MAX_SEGMENTS) has been at least 64 on
	// every kernel that has the feature at all; staying at 64 also means a
	// GSO run never exceeds what one txBatchSize sendmmsg drain hands the
	// flusher anyway.
	maxGSOSegs = 64

	// maxGSOBytes caps a GSO run's total payload. The kernel enforces the
	// IPv4/UDP 16-bit length limit on the pre-segmentation super-datagram;
	// 65000 stays safely under it after headers without needing to reason
	// about v4-vs-v6 header sizes per send.
	maxGSOBytes = 65000

	// groBufSize is the per-slot receive buffer size when GRO is on: a
	// coalesced super-datagram can approach the 16-bit length limit, and a
	// buffer smaller than what the kernel coalesced means truncation
	// (MSG_TRUNC) — silent payload loss, exactly what this path must never
	// risk. 64 KiB slots are why rxBatchSizeGRO (batch_linux.go) shrinks
	// the slot count relative to the non-GRO reader.
	groCtrlLen = 64 // per-message control buffer; UDP_GRO's cmsg needs CmsgSpace(4)=24, the rest is headroom
	groBufSize = 65535

	// rxBatchSizeGRO replaces rxBatchSize for a GRO-enabled reader — see
	// readLoopBatched (batch_linux.go) for the sizing tradeoff.
	rxBatchSizeGRO = 16
)

// probeUDPSegment reports whether this kernel accepts the UDP_SEGMENT socket
// option at all. Setting it to 0 is the documented "disabled" value, so a
// successful call proves support without changing the socket's behaviour;
// pre-4.18 kernels fail with ENOPROTOOPT. One probe is enough per process —
// this is a kernel capability, not a per-socket one — but it is cheap enough
// that initBatch just probes the first socket it has.
func probeUDPSegment(rc syscall.RawConn) bool {
	ok := false
	_ = rc.Control(func(fd uintptr) {
		var zero int32
		_, _, e := syscall.Syscall6(syscall.SYS_SETSOCKOPT, fd,
			solUDP, udpSegment, uintptr(unsafe.Pointer(&zero)), 4, 0)
		ok = e == 0
	})
	return ok
}

// enableUDPGRO turns on socket-level UDP GRO for one socket and reports
// whether the kernel accepted it (pre-5.0 kernels fail with ENOPROTOOPT).
// Unlike the TX side this genuinely changes receive behaviour — the kernel
// may now deliver coalesced buffers — so it is only called when the reader is
// prepared to split them (newBatchReader with gro=true).
func enableUDPGRO(rc syscall.RawConn) bool {
	ok := false
	_ = rc.Control(func(fd uintptr) {
		one := int32(1)
		_, _, e := syscall.Syscall6(syscall.SYS_SETSOCKOPT, fd,
			solUDP, udpGRO, uintptr(unsafe.Pointer(&one)), 4, 0)
		ok = e == 0
	})
	return ok
}

// putSegmentCmsg writes a UDP_SEGMENT control message carrying stride into b
// and returns the number of bytes the msghdr's Controllen should claim:
// CmsgLen(2), since alignment padding is only required BETWEEN cmsgs and
// this buffer carries exactly one (verified against the live kernel by
// TestRawGSOSendSegments). b must be at least segmentCmsgSpace bytes. Layout
// is the 64-bit cmsghdr — u64 len, s32 level, s32 type, then data —
// hand-built for the same reason batch_linux.go hand-builds mmsghdr (no
// dependencies, 64-bit-only build tag), with the sizes taken from the
// stdlib's own CmsgLen/CmsgSpace rather than hardcoded so the alignment
// arithmetic has one source of truth.
func putSegmentCmsg(b []byte, stride uint16) int {
	l := syscall.CmsgLen(2) // cmsghdr + u16 payload, before trailing alignment
	binary.NativeEndian.PutUint64(b[0:8], uint64(l))
	binary.NativeEndian.PutUint32(b[8:12], solUDP)
	binary.NativeEndian.PutUint32(b[12:16], udpSegment)
	binary.NativeEndian.PutUint16(b[16:18], stride)
	return l
}

// segmentCmsgSpace is the buffer size putSegmentCmsg needs (CmsgSpace pads
// CmsgLen to alignment).
var segmentCmsgSpace = syscall.CmsgSpace(2)

// parseGROCmsg walks a received control buffer and returns the UDP_GRO
// stride, or 0 when none is present (the ordinary single-datagram case).
// The kernel's udp_cmsg_recv puts the stride as a 4-byte int, asymmetric
// with the u16 the TX side sends — that is the ABI, not a typo. Defensive
// throughout: a malformed length ends the walk rather than mis-slicing.
func parseGROCmsg(b []byte) int {
	const hdr = syscall.SizeofCmsghdr // 16 on 64-bit
	for len(b) >= hdr {
		clen := int(binary.NativeEndian.Uint64(b[0:8]))
		if clen < hdr || clen > len(b) {
			return 0 // malformed or truncated; treat as no GRO
		}
		level := int32(binary.NativeEndian.Uint32(b[8:12]))
		typ := int32(binary.NativeEndian.Uint32(b[12:16]))
		if level == solUDP && typ == udpGRO && clen >= hdr+4 {
			return int(int32(binary.NativeEndian.Uint32(b[16:20])))
		}
		// Advance to the next cmsg at the aligned boundary.
		next := (clen + 7) &^ 7
		if next <= 0 || next >= len(b) {
			return 0
		}
		b = b[next:]
	}
	return 0
}

// gsoRunLen reports how many consecutive slots starting at index start (ring
// positions start..start+avail-1) form one UDP_SEGMENT-eligible run: same
// destination, every payload the same size as the first, with at most one
// final SHORTER payload allowed as the run's tail (the kernel slices the
// super-datagram at stride boundaries, so only the last segment may be
// short). Returns the run length; a result < 2 means "not worth a GSO send"
// and the caller falls back to the plain path for that slot.
func gsoRunLen(slots []sendSlot, mask uint64, start, avail uint64) uint64 {
	first := &slots[start&mask]
	stride := first.n
	if stride == 0 || stride > maxGSOBytes {
		return 1
	}
	n := uint64(1)
	total := stride
	for n < avail && n < maxGSOSegs {
		s := &slots[(start+n)&mask]
		if s.addr != first.addr || s.n > stride || total+s.n > maxGSOBytes {
			break
		}
		short := s.n < stride
		n++
		total += s.n
		if short {
			break // a short segment can only ever be the last one
		}
	}
	return n
}
