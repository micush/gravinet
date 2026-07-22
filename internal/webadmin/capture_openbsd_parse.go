package webadmin

import "encoding/binary"

// This file deliberately has no _openbsd.go suffix and no openbsd build tag,
// unlike the rest of the OpenBSD capture backend: the byte-offset parsing
// itself has no OS dependency once the offsets are known, and keeping it
// free of the platform restriction is what lets it be unit-tested here
// without an actual OpenBSD box to run bpf(4) on. Only capture_openbsd.go's
// loop (which does the actual read(2) from /dev/bpf) calls this.

// parseBPFHdr reads OpenBSD's bpf_hdr fields directly at their documented
// byte offsets (rather than overlaying a Go struct, to sidestep any Go/C
// struct-padding mismatch) and reports whether buf is long enough to
// contain them. OpenBSD's current struct (sys/net/bpf.h, confirmed against
// $OpenBSD: bpf.h,v 1.74 2025/03/04) is:
//
//	struct bpf_timeval {           // 32-bit-only, unlike FreeBSD's bpf_ts
//	    u_int32_t tv_sec;
//	    u_int32_t tv_usec;
//	};
//	struct bpf_hdr {
//	    struct bpf_timeval bh_tstamp; // offset 0,  8 bytes (tv_sec@0, tv_usec@4)
//	    u_int32_t bh_caplen;          // offset 8
//	    u_int32_t bh_datalen;         // offset 12 (unused here)
//	    u_int16_t bh_hdrlen;          // offset 16
//	    u_int16_t bh_ifidx;           // offset 18 ┐
//	    u_int16_t bh_flowid;          // offset 20 │ OpenBSD-only fields,
//	    u_int8_t  bh_flags;           // offset 22 │ appended after the
//	    u_int8_t  bh_drops;           // offset 23 │ classic 4-field header.
//	    u_int16_t bh_csumflags;       // offset 24 ┘ None read here.
//	};
//
// The offsets of the four fields this actually reads — tv_sec, tv_usec,
// bh_caplen, and bh_hdrlen (0/4/8/16) — are identical to capture_darwin.go's,
// since both platforms use the same 32-bit-only timeval for bh_tstamp. The
// five OpenBSD-only fields appended after bh_hdrlen don't need to be parsed
// at all: bh_hdrlen itself is read from the wire and used as the actual
// offset to the packet payload, never assumed from sizeof(struct bpf_hdr) —
// so that tail (and anything OpenBSD appends to it in a future release) is
// skipped automatically, the same defensive approach capture_freebsd.go
// uses for its own trailing alignment padding.
func parseBPFHdr(buf []byte) (sec, usec int64, caplen uint32, hdrlen uint16, ok bool) {
	if len(buf) < 18 {
		return 0, 0, 0, 0, false
	}
	sec = int64(binary.LittleEndian.Uint32(buf[0:4]))
	usec = int64(binary.LittleEndian.Uint32(buf[4:8]))
	caplen = binary.LittleEndian.Uint32(buf[8:12])
	hdrlen = binary.LittleEndian.Uint16(buf[16:18])
	return sec, usec, caplen, hdrlen, true
}
