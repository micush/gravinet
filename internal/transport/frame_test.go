package transport

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	fc := &frameConn{w: &buf}
	msgs := [][]byte{
		[]byte("a"),
		[]byte("gravinet-frame"),
		bytes.Repeat([]byte{0xab}, 1400), // realistic mesh datagram size
	}
	for _, m := range msgs {
		if err := fc.writeFrame(m); err != nil {
			t.Fatalf("writeFrame(%d bytes): %v", len(m), err)
		}
	}
	// Read them back in order from the same stream.
	r := bytes.NewReader(buf.Bytes())
	for i, want := range msgs {
		got, err := readFrame(r)
		if err != nil {
			t.Fatalf("readFrame #%d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame #%d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
	// Stream is now exhausted: a clean EOF, not a torn frame.
	if _, err := readFrame(r); err != io.EOF {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
}

func TestWriteFrameEmptyIsNoop(t *testing.T) {
	var buf bytes.Buffer
	fc := &frameConn{w: &buf}
	if err := fc.writeFrame(nil); err != nil {
		t.Fatalf("writeFrame(nil): %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty frame wrote %d bytes, want 0", buf.Len())
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	fc := &frameConn{w: &buf}
	if err := fc.writeFrame(bytes.Repeat([]byte{0}, maxFrameLen)); err == nil {
		t.Fatal("expected error for oversized frame, got nil")
	}
}

func TestReadFrameZeroLengthRejected(t *testing.T) {
	// A 2-byte length header of 0 is malformed (we never write empty frames).
	r := bytes.NewReader([]byte{0x00, 0x00})
	if _, err := readFrame(r); err == nil {
		t.Fatal("expected error for zero-length frame header, got nil")
	}
}

func TestReadFrameTruncated(t *testing.T) {
	// Header claims 10 bytes but only 3 follow: must error, not hang or return short.
	r := bytes.NewReader([]byte{0x00, 0x0a, 1, 2, 3})
	if _, err := readFrame(r); err == nil {
		t.Fatal("expected error for truncated frame, got nil")
	}
}
