package webadmin

import "time"

// This file deliberately has no build tag, unlike the platform loops that
// call it: the batch-walking logic itself has no OS dependency once
// parseBPFHdr's offsets and the platform's BPF_ALIGNMENT are known, and
// keeping it free of any platform restriction is what lets it be
// unit-tested here without an actual Darwin or OpenBSD box — the same
// reasoning capture_openbsd_parse.go's own doc comment gives for splitting
// parseBPFHdr out the same way.
//
// Before this existed, capture_darwin.go's loop() and capture_openbsd.go's
// loop() each carried their own hand-written copy of this exact walk —
// identical in every respect except which BPF_ALIGNMENT constant to round
// up to, since both platforms share the same 32-bit-only bpf_hdr layout
// parseBPFHdr already reads. They quietly diverged anyway: OpenBSD's copy
// used the correct value (4, sizeof(u_int32_t)) with a doc comment
// explicitly warning not to use FreeBSD's 8-byte one — but Darwin's
// independent copy used 8 regardless, undocumented, apparently carried
// over from FreeBSD's genuinely-different (8-byte-timeval) file it was
// likely adapted from. A real six-node mesh capture bundle turned this up:
// gn-macos.pcap had every other record decode to a plausible-looking but
// wrong timestamp, with a garbage payload underneath it (nonsense address
// family, no valid IP version nibble) — not a corrupted file, a
// misaligned one. A packet whose true padded slot already happens to be a
// multiple of 8 parses fine either way; only a slot that lands strictly
// between the 4-byte and 8-byte alignment boundaries walks the read
// pointer 4 bytes into the following record instead of its start, which
// is exactly the "half garbage, half fine" shape a real bundle showed,
// not a clean, loud failure. One implementation, parameterized by the one
// thing that actually differs, closes off that whole class of drift: get
// wordAlign right in one place and both platforms inherit it correctly, or
// get it wrong in one place and a test here catches it on any machine —
// see capture_bpfbatch_test.go, in particular
// TestParseBPFBatchWrongAlignmentMisparsesOddSlot, which fails against the
// Darwin bug's own wordAlign=8 and passes at 4, proving this isn't a
// theoretical distinction.
//
// wordAlign is BPF_ALIGNMENT for the platform buf came from: 4 on Darwin
// and OpenBSD (confirmed against each OS's current bpf.h — both define
// BPF_ALIGNMENT as sizeof(u_int32_t)/sizeof(int32_t), not sizeof(long),
// specifically because that keeps the on-the-wire BPF layout independent
// of whether the reading process is 32- or 64-bit), 8 on FreeBSD — which
// also uses a different, wider bpf_hdr shape entirely (see
// capture_freebsd.go's own file header comment) and so does not call this
// function at all.
func parseBPFBatch(buf []byte, n int, wordAlign int, onPacket func(time.Time, []byte)) {
	p := 0
	for p+18 <= n {
		sec, usec, caplen, hdrlen, ok := parseBPFHdr(buf[p:n])
		if !ok {
			return
		}
		start := p + int(hdrlen)
		end := start + int(caplen)
		if hdrlen == 0 || end > n || end <= start {
			return // malformed/short trailing record; stop this batch
		}
		pkt := make([]byte, caplen)
		copy(pkt, buf[start:end])
		onPacket(time.Unix(sec, usec*1000), pkt)
		slot := int(hdrlen) + int(caplen)
		p += (slot + wordAlign - 1) &^ (wordAlign - 1)
	}
}
