package crypto

import (
	"bytes"
	"sort"
	"sync"
	"testing"
)

func sessionPair(t testing.TB) (*Session, *Session) {
	ie, _ := NewEphemeral()
	re, _ := NewEphemeral()
	si, _ := ie.Shared(re.Public())
	sr, _ := re.Shared(ie.Public())
	psk, _ := GenerateKey()
	raw, _ := DecodeKey(psk)
	ki := DeriveSessionKeys(si, raw, []byte("tx"), true)
	kr := DeriveSessionKeys(sr, raw, []byte("tx"), false)
	a, _ := NewSession(ki)
	b, _ := NewSession(kr)
	return a, b
}

// TestSealOpenInPlace mirrors the engine's hot path: encrypt the inner frame in
// place inside one buffer (header room + innerType + body), ship it, then decrypt
// in place on the receiver. Verifies the aliasing is correct end to end.
func TestSealOpenInPlace(t *testing.T) {
	tx, rx := sessionPair(t)
	const H = 14 // DataHeaderLen
	body := bytes.Repeat([]byte("overlay-packet-"), 40)
	buf := make([]byte, H+1+len(body)+16)
	buf = buf[:H+1+len(body)]
	buf[H] = 0x00 // innerIP
	copy(buf[H+1:], body)
	aad := []byte{0x01, 0x10, 0xde, 0xad, 0xbe, 0xef}

	pt := buf[H:]
	ctr, ct := tx.Seal(pt[:0], pt, aad) // in place; ct aliases buf[H:]

	wire := append([]byte(nil), ct...) // simulate transmission into a fresh RX buffer
	got, err := rx.Open(wire[:0], wire, aad, ctr)
	if err != nil {
		t.Fatalf("in-place open: %v", err)
	}
	if got[0] != 0x00 || !bytes.Equal(got[1:], body) {
		t.Fatalf("in-place round-trip mismatch")
	}
}

// TestSealConcurrentUniqueCounters validates the lock-free atomic send counter:
// concurrent Seals must each get a distinct counter and all must decrypt.
func TestSealConcurrentUniqueCounters(t *testing.T) {
	tx, rx := sessionPair(t)
	const n = 2000
	type pkt struct {
		ctr uint64
		ct  []byte
	}
	out := make([]pkt, n)
	var wg sync.WaitGroup
	aad := []byte("aad")
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := []byte{byte(i), byte(i >> 8), 0xAB}
			ctr, ct := tx.Seal(nil, msg, aad)
			out[i] = pkt{ctr, ct}
		}(i)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ctr < out[j].ctr })
	seen := map[uint64]bool{}
	for _, p := range out {
		if seen[p.ctr] {
			t.Fatalf("duplicate counter %d — atomic increment broken", p.ctr)
		}
		seen[p.ctr] = true
		if _, err := rx.Open(nil, p.ct, aad, p.ctr); err != nil {
			t.Fatalf("open ctr %d: %v", p.ctr, err)
		}
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique counters, got %d", n, len(seen))
	}
}

func BenchmarkSealInPlace(b *testing.B) {
	tx, _ := sessionPair(b)
	const H = 14
	body := make([]byte, 1400)
	buf := make([]byte, H+1+len(body)+16)
	aad := []byte{1, 2, 3, 4, 5, 6}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bb := buf[:H+1+len(body)]
		bb[H] = 0
		pt := bb[H:]
		_, _ = tx.Seal(pt[:0], pt, aad)
	}
}

// BenchmarkSealAllocating reproduces the previous hot path (fresh frame, dst=nil
// seal, fresh output) for a side-by-side allocation comparison.
func BenchmarkSealAllocating(b *testing.B) {
	tx, _ := sessionPair(b)
	body := make([]byte, 1400)
	aad := []byte{1, 2, 3, 4, 5, 6}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frame := make([]byte, 1+len(body))
		frame[0] = 0
		copy(frame[1:], body)
		_, ct := tx.Seal(nil, frame, aad)
		out := make([]byte, 14+len(ct))
		copy(out[14:], ct)
		_ = out
	}
}
