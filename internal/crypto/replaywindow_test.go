package crypto

import (
	"math/rand"
	"testing"
)

// newReplayPair returns a sealing cipher and the opening cipher that matches
// it, both keyed identically (the replay window is what's under test, not the
// key schedule).
func newReplayPair(t *testing.T) (send, recv *Cipher) {
	t.Helper()
	s, err := newCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

type sealed struct {
	ctr uint64
	ct  []byte
}

func sealN(t *testing.T, c *Cipher, n int) []sealed {
	t.Helper()
	out := make([]sealed, n)
	for i := range out {
		ctr, ct := c.Seal(nil, []byte{byte(i), byte(i >> 8)}, nil)
		out[i] = sealed{ctr, ct}
	}
	return out
}

// The regression this change exists for: reordering that would have exceeded
// the old 64-packet window must now be absorbed. Delivering a run of packets
// in fully reversed order is the worst case — the last-sealed arrives first,
// so every subsequent one is maximally late.
func TestReplayWindowAbsorbsDeepReordering(t *testing.T) {
	send, recv := newReplayPair(t)
	pkts := sealN(t, send, replayWindowSize)
	for i := len(pkts) - 1; i >= 0; i-- {
		if _, err := recv.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != nil {
			t.Fatalf("packet %d (%d behind the head) rejected: %v", i, len(pkts)-1-i, err)
		}
	}
	// Every one of them must still be rejected on a genuine replay.
	for i := range pkts {
		if _, err := recv.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != ErrReplay {
			t.Fatalf("replay of packet %d not rejected: %v", i, err)
		}
	}
}

// A packet exactly at the trailing edge is still accepted; one past it is not.
func TestReplayWindowBoundary(t *testing.T) {
	send, recv := newReplayPair(t)
	pkts := sealN(t, send, replayWindowSize+2)

	// Advance the head to the last counter.
	last := len(pkts) - 1
	if _, err := recv.Open(nil, pkts[last].ct, nil, pkts[last].ctr); err != nil {
		t.Fatalf("head packet rejected: %v", err)
	}
	// replayWindowSize-1 behind the head: inside the window.
	edge := last - (replayWindowSize - 1)
	if _, err := recv.Open(nil, pkts[edge].ct, nil, pkts[edge].ctr); err != nil {
		t.Fatalf("packet at the trailing edge (%d behind) rejected: %v", replayWindowSize-1, err)
	}
	// replayWindowSize behind the head: outside.
	past := last - replayWindowSize
	if _, err := recv.Open(nil, pkts[past].ct, nil, pkts[past].ctr); err != ErrReplay {
		t.Fatalf("packet %d behind the head was accepted, want ErrReplay: %v", replayWindowSize, err)
	}
}

// The ring is indexed by counter/64 modulo replayWords, so words get reused
// every replayWindowSize counters. If advancing past a word boundary fails to
// clear the word it is about to reuse, bits from a lap ago read as "already
// seen" and reject perfectly good packets. This walks well past several laps.
func TestReplayWindowRingWrapDoesNotRejectFreshPackets(t *testing.T) {
	send, recv := newReplayPair(t)
	for i := 0; i < replayWindowSize*4; i++ {
		ctr, ct := send.Seal(nil, []byte{1}, nil)
		if _, err := recv.Open(nil, ct, nil, ctr); err != nil {
			t.Fatalf("in-order packet %d (counter %d) rejected after wrap: %v", i, ctr, err)
		}
	}
}

// A large forward jump — the far side restarted its counter far ahead, or a
// long burst was lost — must invalidate the whole ring rather than leaving
// stale bits that reject subsequent legitimate packets.
func TestReplayWindowLargeForwardJumpClearsRing(t *testing.T) {
	send, recv := newReplayPair(t)
	// Seed the ring densely.
	for i := 0; i < replayWindowSize; i++ {
		ctr, ct := send.Seal(nil, []byte{1}, nil)
		if _, err := recv.Open(nil, ct, nil, ctr); err != nil {
			t.Fatalf("seed packet %d rejected: %v", i, err)
		}
	}
	// Jump several laps ahead. The nonce is derived from the counter, so the
	// counter can't be fabricated at Open time — drive the sender's own
	// counter forward instead, exactly as a peer that restarted far ahead or
	// lost a long burst would.
	const jump = uint64(replayWindowSize) * 10
	send.counter = jump
	var firstCT []byte
	for i := uint64(0); i < 200; i++ {
		ctr, ct := send.Seal(nil, []byte{2}, nil)
		if ctr != jump+i {
			t.Fatalf("counter %d, want %d", ctr, jump+i)
		}
		if i == 0 {
			firstCT = append([]byte(nil), ct...)
		}
		if _, err := recv.Open(nil, ct, nil, ctr); err != nil {
			t.Fatalf("packet at counter %d after a %d jump rejected: %v", ctr, jump, err)
		}
	}
	// A replay from immediately after the jump is still caught.
	if _, err := recv.Open(nil, firstCT, nil, jump); err != ErrReplay {
		t.Fatalf("replay after a large jump was accepted: %v", err)
	}
}

// Randomised: shuffle within a bounded reordering distance, which is what the
// send-side worker pool actually produces, and assert every packet is
// delivered exactly once — no false rejection, no double accept.
func TestReplayWindowShuffledDeliveryAcceptsEachExactlyOnce(t *testing.T) {
	send, recv := newReplayPair(t)
	const total = replayWindowSize * 3
	pkts := sealN(t, send, total)

	rng := rand.New(rand.NewSource(1))
	// Shuffling within fixed blocks keeps every packet's displacement under
	// the block size while still visiting each exactly once. (A whole-slice
	// Perm would move packets arbitrarily far and legitimately drop some,
	// which would prove nothing about the window.)
	const block = replayWindowSize / 2
	var order []int
	for start := 0; start < len(pkts); start += block {
		end := start + block
		if end > len(pkts) {
			end = len(pkts)
		}
		for _, j := range rng.Perm(end - start) {
			order = append(order, start+j)
		}
	}
	accepted := make(map[uint64]int)
	for _, i := range order {
		if _, err := recv.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != nil {
			t.Fatalf("packet with counter %d rejected under bounded reordering: %v", pkts[i].ctr, err)
		}
		accepted[pkts[i].ctr]++
	}
	if len(accepted) != len(pkts) {
		t.Fatalf("accepted %d distinct counters, want %d", len(accepted), len(pkts))
	}
	for ctr, n := range accepted {
		if n != 1 {
			t.Fatalf("counter %d accepted %d times", ctr, n)
		}
	}
}

// The security property, stated directly: widening the window must not let any
// counter through twice, at any offset inside it.
func TestReplayWindowNeverAcceptsACounterTwice(t *testing.T) {
	send, recv := newReplayPair(t)
	pkts := sealN(t, send, replayWindowSize)
	for i := range pkts {
		if _, err := recv.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != nil {
			t.Fatalf("first delivery of %d failed: %v", i, err)
		}
	}
	for i := range pkts {
		if _, err := recv.Open(nil, pkts[i].ct, nil, pkts[i].ctr); err != ErrReplay {
			t.Fatalf("second delivery of counter %d was not rejected: %v", pkts[i].ctr, err)
		}
	}
}
