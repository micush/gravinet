package webadmin

import (
	"encoding/binary"
	"testing"
	"time"
)

// buildBPFRecordAt appends one synthetic bpf_hdr+payload record to buf,
// padding the whole slot up to wordAlign — the same shape a real kernel's
// BPF device delivers, used here to build multi-record batches without a
// real Darwin/OpenBSD box (mirrors capture_openbsd_parse_test.go's own
// synthetic-buffer approach, extended to a full multi-record batch since
// what's under test here is the record-to-record *advance*, not a single
// header's field offsets).
func buildBPFRecordAt(buf []byte, sec, usec int64, payload []byte, wordAlign int) []byte {
	const hdrlen = 18
	rec := make([]byte, hdrlen+len(payload))
	binary.LittleEndian.PutUint32(rec[0:4], uint32(sec))
	binary.LittleEndian.PutUint32(rec[4:8], uint32(usec))
	binary.LittleEndian.PutUint32(rec[8:12], uint32(len(payload)))  // bh_caplen
	binary.LittleEndian.PutUint32(rec[12:16], uint32(len(payload))) // bh_datalen
	binary.LittleEndian.PutUint16(rec[16:18], uint16(hdrlen))
	copy(rec[hdrlen:], payload)
	padded := (len(rec) + wordAlign - 1) &^ (wordAlign - 1)
	slot := make([]byte, padded)
	copy(slot, rec)
	return append(buf, slot...)
}

type wantBPFRecord struct {
	sec, usec int64
	payload   []byte
}

// TestParseBPFBatchDecodesMixedAlignmentSlots builds a batch the way a real
// Darwin/OpenBSD kernel would — records padded to a 4-byte boundary — where
// at least one record's true slot size (header+payload) is a multiple of 4
// but not 8. That's the exact shape a real six-node mesh capture bundle
// turned up misparsed: capture_darwin.go previously rounded advances to 8
// bytes regardless, which is a no-op for a slot that's already a multiple
// of 8 and silently wrong for one that isn't. Confirms every record decodes
// with its original timestamp and payload when parsed at the correct
// alignment (4).
func TestParseBPFBatchDecodesMixedAlignmentSlots(t *testing.T) {
	want := []wantBPFRecord{
		{1785650119, 257828, []byte("0123456789")}, // 10-byte payload -> slot 28: a multiple of 4, not 8
		{1785650119, 866541, []byte("abcdef")},     // 6-byte payload -> slot 24: a multiple of both
		{1785650120, 184986, []byte("9876543210")}, // 10-byte payload -> slot 28 again
	}

	var buf []byte
	for _, w := range want {
		buf = buildBPFRecordAt(buf, w.sec, w.usec, w.payload, 4)
	}

	got := collectBPFBatch(buf, 4)
	if len(got) != len(want) {
		t.Fatalf("decoded %d records, want %d", len(got), len(want))
	}
	for i, w := range want {
		wantT := time.Unix(w.sec, w.usec*1000)
		if !got[i].t.Equal(wantT) {
			t.Errorf("record %d: t = %v, want %v", i, got[i].t, wantT)
		}
		if string(got[i].data) != string(w.payload) {
			t.Errorf("record %d: payload = %q, want %q", i, got[i].data, w.payload)
		}
	}
}

// TestParseBPFBatchWrongAlignmentMisparsesOddSlot is the negative case: the
// exact same shape of batch as above, parsed with wordAlign=8 — the value
// capture_darwin.go used before this fix. Proves the alignment value isn't
// cosmetic: because the first record's true slot (28 bytes) is a multiple
// of 4 but not 8, rounding its advance up to 8 overshoots into the middle
// of the second record's header instead of landing on its start, so the
// second record does not come back with the timestamp or payload it was
// built with (either misdecoded, or the batch gives up early on what now
// looks like a malformed trailing record — parseBPFBatch's own contract
// treats both as "stop here" rather than fabricating something). This is
// the test that would have caught the reported bug directly: a header-
// offset-only test (TestParseBPFHdr) can't distinguish a wrong alignment
// from a right one, since both parse a single, correctly-positioned header
// identically.
func TestParseBPFBatchWrongAlignmentMisparsesOddSlot(t *testing.T) {
	rec0 := wantBPFRecord{1785650119, 257828, []byte("0123456789")} // slot 28: 4-aligned, not 8-aligned
	rec1 := wantBPFRecord{1785650119, 866541, []byte("abcdef")}

	var buf []byte
	buf = buildBPFRecordAt(buf, rec0.sec, rec0.usec, rec0.payload, 4)
	buf = buildBPFRecordAt(buf, rec1.sec, rec1.usec, rec1.payload, 4)

	got := collectBPFBatch(buf, 8)

	wantRec1T := time.Unix(rec1.sec, rec1.usec*1000)
	correct := len(got) == 2 && got[1].t.Equal(wantRec1T) && string(got[1].data) == string(rec1.payload)
	if correct {
		t.Fatal("wordAlign=8 decoded the second record correctly — this test no longer exercises the alignment bug it's named for; the synthetic slot sizes above may need adjusting")
	}
	// The first record's own header sits at a correct, unaffected offset
	// regardless of wordAlign (only the *advance* is wrong), so it should
	// still come back fine — confirming the failure is specifically about
	// record-to-record sync, not a general parsing breakage.
	if len(got) < 1 || !got[0].t.Equal(time.Unix(rec0.sec, rec0.usec*1000)) || string(got[0].data) != string(rec0.payload) {
		t.Error("the first record (unaffected by the advance bug) should still have decoded correctly")
	}
}

// TestParseBPFBatchStopsOnMalformedTrailingRecord proves a truncated header
// at the tail of a batch stops cleanly — parses whatever came before it and
// calls onPacket no further — instead of panicking or reading out of
// bounds. This is the ordinary, expected shape of the last, partial record
// at the end of a live BPF read, not just a corruption scenario.
func TestParseBPFBatchStopsOnMalformedTrailingRecord(t *testing.T) {
	var buf []byte
	buf = buildBPFRecordAt(buf, 1785650119, 0, []byte("hello"), 4)
	buf = append(buf, 0x01, 0x02, 0x03) // a few trailing bytes short of a full 18-byte header

	got := collectBPFBatch(buf, 4)
	if len(got) != 1 {
		t.Errorf("decoded %d records, want exactly 1 (the malformed trailer should stop the batch, not panic or fabricate a record)", len(got))
	}
}

// TestParseBPFBatchEmptyBuffer is the degenerate case: nothing to parse,
// onPacket never called, no panic.
func TestParseBPFBatchEmptyBuffer(t *testing.T) {
	got := collectBPFBatch(nil, 4)
	if len(got) != 0 {
		t.Errorf("decoded %d records from an empty buffer, want 0", len(got))
	}
}

type capturedBPFRecord struct {
	t    time.Time
	data []byte
}

// collectBPFBatch runs parseBPFBatch and captures every record it reports,
// copying the payload since parseBPFBatch's own contract (like
// capture_darwin.go/capture_openbsd.go's real onPacket callbacks) doesn't
// promise the slice stays valid or unaliased beyond the callback.
func collectBPFBatch(buf []byte, wordAlign int) []capturedBPFRecord {
	var got []capturedBPFRecord
	parseBPFBatch(buf, len(buf), wordAlign, func(ts time.Time, data []byte) {
		got = append(got, capturedBPFRecord{t: ts, data: append([]byte(nil), data...)})
	})
	return got
}
