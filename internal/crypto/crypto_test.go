package crypto

import (
	"bytes"
	"testing"
)

func TestKeyIDMatchesAcrossSlots(t *testing.T) {
	// Same key in different "slot" positions must produce the same KeyID, which
	// is what lets two hosts authenticate without slot alignment.
	k, _ := GenerateKey()
	hostA, err := NewKeySet([]string{"", "", k}) // key in slot 2
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := NewKeySet([]string{k, "", ""}) // same key in slot 0
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := DecodeKey(k)
	id := DeriveKeyID(raw)
	if _, ok := hostA.Lookup(id); !ok {
		t.Fatal("host A should match its own key by id")
	}
	if _, ok := hostB.Lookup(id); !ok {
		t.Fatal("host B should match the same key in a different slot")
	}
}

func TestKeyIDMismatch(t *testing.T) {
	ka, _ := GenerateKey()
	kb, _ := GenerateKey()
	a, _ := NewKeySet([]string{ka})
	rawB, _ := DecodeKey(kb)
	if _, ok := a.Lookup(DeriveKeyID(rawB)); ok {
		t.Fatal("disjoint key sets must not match")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	// Simulate a handshake producing agreed directional keys on both ends.
	initEph, _ := NewEphemeral()
	respEph, _ := NewEphemeral()
	sharedI, _ := initEph.Shared(respEph.Public())
	sharedR, _ := respEph.Shared(initEph.Public())
	if !bytes.Equal(sharedI, sharedR) {
		t.Fatal("X25519 shared secrets differ")
	}
	psk, _ := GenerateKey()
	pskRaw, _ := DecodeKey(psk)
	transcript := []byte("transcript-bytes")

	ki := DeriveSessionKeys(sharedI, pskRaw, transcript, true)
	kr := DeriveSessionKeys(sharedR, pskRaw, transcript, false)
	// initiator's send key must equal responder's receive key and vice versa.
	if !bytes.Equal(ki.Send, kr.Receive) || !bytes.Equal(ki.Receive, kr.Send) {
		t.Fatal("directional keys do not cross-match")
	}

	initSess, _ := NewSession(ki)
	respSess, _ := NewSession(kr)

	msg := []byte("hello over the overlay")
	aad := []byte("header-aad")
	ctr, ct := initSess.Seal(nil, msg, aad)
	got, err := respSess.Open(nil, ct, aad, ctr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestAADTamperDetected(t *testing.T) {
	psk, _ := GenerateKey()
	pskRaw, _ := DecodeKey(psk)
	k := DeriveSessionKeys(make([]byte, 32), pskRaw, nil, true)
	s, _ := NewSession(k)
	r, _ := NewSession(DeriveSessionKeys(make([]byte, 32), pskRaw, nil, false))
	ctr, ct := s.Seal(nil, []byte("x"), []byte("aad1"))
	if _, err := r.Open(nil, ct, []byte("aad2"), ctr); err == nil {
		t.Fatal("tampered AAD must fail authentication")
	}
}

func TestReplayWindow(t *testing.T) {
	c, _ := newCipher(make([]byte, 32))
	open, _ := newCipher(make([]byte, 32))

	// Seal a few packets, deliver out of order, then replay.
	var pkts []struct {
		ctr uint64
		ct  []byte
	}
	for i := 0; i < 5; i++ {
		ctr, ct := c.Seal(nil, []byte{byte(i)}, nil)
		pkts = append(pkts, struct {
			ctr uint64
			ct  []byte
		}{ctr, ct})
	}
	// deliver 0,2,1,4,3 — all should succeed once.
	order := []int{0, 2, 1, 4, 3}
	for _, i := range order {
		if _, err := open.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != nil {
			t.Fatalf("packet %d should open: %v", i, err)
		}
	}
	// replay packet 2 — must be rejected.
	if _, err := open.Open(nil, pkts[2].ct, nil, pkts[2].ctr); err != ErrReplay {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestHandshakeSealOpen(t *testing.T) {
	key, _ := GenerateKey()
	raw, _ := DecodeKey(key)
	body := []byte("ephemeral-pub|hostname|ts")
	aad := []byte("hs-header")
	sealed, err := SealWithKey(raw, body, aad)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenWithKey(raw, sealed, aad)
	if err != nil {
		t.Fatalf("open handshake: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("handshake body mismatch")
	}
	// wrong key must fail.
	other, _ := GenerateKey()
	otherRaw, _ := DecodeKey(other)
	if _, err := OpenWithKey(otherRaw, sealed, aad); err == nil {
		t.Fatal("handshake must not open under the wrong key")
	}
}
