package protocol

import (
	"bytes"
	"testing"

	"gravinet/internal/crypto"
)

func TestDataHeaderRoundTrip(t *testing.T) {
	buf := make([]byte, DataHeaderLen+32)
	h := DataHeader{RecvSession: 0xDEADBEEF, Counter: 0x0102030405060708}
	n := EncodeData(buf, h)
	if n != DataHeaderLen {
		t.Fatalf("encoded %d bytes, want %d", n, DataHeaderLen)
	}
	// pad ciphertext region so length checks pass.
	pkt := append(buf[:DataHeaderLen], make([]byte, GCMOverhead)...)
	got, aad, ct, err := DecodeData(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("header mismatch: %+v vs %+v", got, h)
	}
	if !bytes.Equal(aad, pkt[:DataAADLen]) {
		t.Fatal("aad should be the pre-counter header portion")
	}
	if len(ct) != GCMOverhead {
		t.Fatalf("ciphertext len %d", len(ct))
	}
}

func TestHSInitRoundTrip(t *testing.T) {
	key, _ := crypto.GenerateKey()
	raw, _ := crypto.DecodeKey(key)
	h := HSInitHeader{Network: 0x1122334455667788, KeyID: crypto.DeriveKeyID(raw)}
	buf := make([]byte, HSInitHeaderLen+16)
	EncodeHSInit(buf, h)
	got, aad, body, err := DecodeHSInit(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != h.Network || got.KeyID != h.KeyID {
		t.Fatalf("hs header mismatch: %+v vs %+v", got, h)
	}
	if len(aad) != HSInitHeaderLen {
		t.Fatalf("aad len %d", len(aad))
	}
	if len(body) != 16 {
		t.Fatalf("body len %d", len(body))
	}
}

func TestPacketTypeAndVersion(t *testing.T) {
	if _, err := PacketType([]byte{0x00, 0x01}); err != ErrVersion {
		t.Fatal("expected version error")
	}
	ty, err := PacketType([]byte{Version, byte(TypeHSResp)})
	if err != nil || ty != TypeHSResp {
		t.Fatalf("got %v %v", ty, err)
	}
	if _, _, _, err := DecodeData([]byte{Version}); err != ErrShort {
		t.Fatal("expected short error")
	}
}
