package webadmin

import (
	"encoding/binary"
	"testing"
)

// TestParseBPFHdr uses a synthetic buffer shaped like OpenBSD's current
// (post-CP_SPIN-era) 26-byte bpf_hdr, including the five OpenBSD-only fields
// appended after bh_hdrlen (bh_ifidx/bh_flowid/bh_flags/bh_drops/
// bh_csumflags), to confirm those trailing fields are correctly ignored
// rather than accidentally misread as part of what this function returns.
func TestParseBPFHdr(t *testing.T) {
	buf := make([]byte, 26)
	binary.LittleEndian.PutUint32(buf[0:4], 1234567890) // bh_tstamp.tv_sec
	binary.LittleEndian.PutUint32(buf[4:8], 500000)     // bh_tstamp.tv_usec
	binary.LittleEndian.PutUint32(buf[8:12], 60)        // bh_caplen
	binary.LittleEndian.PutUint32(buf[12:16], 1500)     // bh_datalen (unused)
	binary.LittleEndian.PutUint16(buf[16:18], 28)       // bh_hdrlen
	// bytes 18-25 (bh_ifidx, bh_flowid, bh_flags, bh_drops, bh_csumflags) are
	// deliberately left as non-zero-looking garbage below to prove they
	// can't leak into the parsed result.
	for i := 18; i < 26; i++ {
		buf[i] = 0xff
	}

	sec, usec, caplen, hdrlen, ok := parseBPFHdr(buf)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if sec != 1234567890 {
		t.Errorf("sec = %d, want 1234567890", sec)
	}
	if usec != 500000 {
		t.Errorf("usec = %d, want 500000", usec)
	}
	if caplen != 60 {
		t.Errorf("caplen = %d, want 60", caplen)
	}
	if hdrlen != 28 {
		t.Errorf("hdrlen = %d, want 28", hdrlen)
	}
}

// TestParseBPFHdrTooShort ensures a buffer too short to contain the fields
// this reads fails closed (ok=false) rather than panicking or returning
// garbage.
func TestParseBPFHdrTooShort(t *testing.T) {
	for _, n := range []int{0, 1, 8, 17} {
		if _, _, _, _, ok := parseBPFHdr(make([]byte, n)); ok {
			t.Errorf("parseBPFHdr(%d-byte buf) should fail, got ok=true", n)
		}
	}
}
