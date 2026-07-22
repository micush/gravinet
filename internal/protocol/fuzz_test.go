package protocol

import "testing"

// These fuzzers assert the wire decoders never panic on arbitrary input — the
// outer header parsers are the first thing hostile UDP touches.

func seed(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1})
	// A well-formed-ish data header.
	d := make([]byte, DataHeaderLen+16)
	EncodeData(d, DataHeader{RecvSession: 7, Counter: 42})
	f.Add(d)
	// A well-formed-ish HS init header.
	h := make([]byte, HSInitHeaderLen+32)
	EncodeHSInit(h, HSInitHeader{Network: 0x1234})
	f.Add(h)
}

func FuzzDecodeData(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) { _, _, _, _ = DecodeData(b) })
}

func FuzzDecodeHSInit(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) { _, _, _, _ = DecodeHSInit(b) })
}

func FuzzDecodeHSResp(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) { _, _, _, _ = DecodeHSResp(b) })
}

func FuzzPacketType(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = PacketType(b) })
}
