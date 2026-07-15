package crypto

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentSealNoCollision is the direct, fast counterpart to the
// mesh package's slower, network-timing-based integration tests: gravinet's
// outbound worker pool (internal/mesh's tunLoop/processOutbound) can now
// call Seal on the *same* peer session's Cipher from multiple goroutines at
// once — packets to one destination read from the TUN device back-to-back
// may be picked up by different workers. This proves, directly and
// repeatably (no network, no timing, sub-second even under -race), the
// property that guarantees that's safe: Cipher.Seal's counter allocation
// (atomic.AddUint64) hands out a strictly unique, monotonic counter per
// call no matter how many goroutines call it simultaneously, and the AEAD
// itself has no shared mutable state a concurrent call could corrupt — so
// every sealed packet decrypts correctly and no two ever share a counter.
// Verification opens strictly in ascending counter order (see below) —
// intentionally decoupled from the concurrent, effectively-random order
// results arrive from the goroutines that sealed them, since the 64-wide
// replay window is designed for realistic network reordering, not
// verifying thousands of results in an arbitrary interleaving after the
// fact.
func TestConcurrentSealNoCollision(t *testing.T) {
	tx, rx := sessionPair(t)

	const goroutines = 32
	const perGoroutine = 200
	total := goroutines * perGoroutine

	type sealed struct {
		counter uint64
		ct      []byte
		orig    []byte
	}
	results := make(chan sealed, total)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				body := []byte(fmt.Sprintf("worker-%d-packet-%d", g, i))
				counter, ct := tx.Seal(nil, body, nil)
				results <- sealed{counter, ct, body}
			}
		}(g)
	}
	wg.Wait()
	close(results)

	seenCounters := make(map[uint64]bool, total)
	bySeal := make(map[uint64]sealed, total)
	got := 0
	for r := range results {
		got++
		if seenCounters[r.counter] {
			t.Fatalf("counter %d used twice by concurrent Seal calls", r.counter)
		}
		seenCounters[r.counter] = true
		bySeal[r.counter] = r
	}
	if got != total {
		t.Fatalf("got %d sealed results, want %d", got, total)
	}
	// Counters must form a dense 0..total-1 run: atomic.AddUint64 guarantees
	// uniqueness and monotonicity, so goroutines*perGoroutine calls must
	// have claimed exactly that range with no gaps or duplicates.
	for c := uint64(0); c < uint64(total); c++ {
		if !seenCounters[c] {
			t.Fatalf("counter %d never claimed — %d total calls should have produced a dense 0..%d run", c, total, total-1)
		}
	}
	// Open in ascending counter order — the replay window (64 wide) is
	// designed to tolerate realistic network reordering, not "verify
	// thousands of results in the arbitrary interleaving 32 concurrent
	// goroutines happened to finish and land in a channel," which isn't how
	// counters are ever consumed for real: a receiver processes packets
	// roughly in arrival order. Opening out of that order here previously
	// made this test itself flaky (a legitimate "replayed or stale" once
	// results landed more than 64 apart from their neighbors), not a sign
	// of anything wrong with Seal/Open's actual concurrency safety, which
	// the dense-counter check above already establishes independently.
	for c := uint64(0); c < uint64(total); c++ {
		r := bySeal[c]
		pt, err := rx.Open(nil, r.ct, nil, r.counter)
		if err != nil {
			t.Fatalf("Open failed for counter %d: %v", r.counter, err)
		}
		if !bytes.Equal(pt, r.orig) {
			t.Fatalf("counter %d: decrypted %q, want %q", r.counter, pt, r.orig)
		}
	}
}
