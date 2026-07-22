package mesh

import (
	"sync"
	"testing"
	"time"
)

func TestByteBucketAllowN(t *testing.T) {
	var now time.Time
	b := newTokenBucket(5000, 5000) // 5000 B/s, burst 5000
	b.now = func() time.Time { return now }
	now = time.Unix(500, 0)
	b.last = now

	// Burst: 5×1000 bytes pass, the 6th does not (frozen clock).
	for i := 0; i < 5; i++ {
		if !b.allowN(1000) {
			t.Fatalf("byte %d should pass within burst", i*1000)
		}
	}
	if b.allowN(1000) {
		t.Fatal("over-burst bytes should be denied while frozen")
	}
	// 1s later: +5000 bytes, capped at burst.
	now = now.Add(time.Second)
	passed := 0
	for i := 0; i < 20; i++ {
		if b.allowN(1000) {
			passed++
		}
	}
	if passed != 5 {
		t.Fatalf("after refill expected 5000 bytes (burst cap), got %d×1000", passed)
	}
}

func TestShaperPacing(t *testing.T) {
	const rate = 200000 // 200 KB/s
	var (
		mu   sync.Mutex
		sent int
	)
	done := make(chan struct{})
	const total = 40
	s := newShaper(rate, 0, 0, nil, func(_ *peerSession, _ []byte) {
		mu.Lock()
		sent++
		if sent == total {
			close(done)
		}
		mu.Unlock()
	})
	go s.run()
	defer s.close()

	start := time.Now()
	pkt := make([]byte, 5000) // ~5031 bytes on the wire
	for i := 0; i < total; i++ {
		if !s.enqueue(nil, pkt) {
			t.Fatalf("enqueue %d unexpectedly dropped", i)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shaper did not drain in time")
	}
	elapsed := time.Since(start)

	// ~40×5031 = 201240 bytes at 200000 B/s ≈ 1.0s (minus one burst). Expect the
	// shaper to have meaningfully paced it, not dumped it instantly.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("traffic was not throttled (drained in %v)", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("shaper too slow: %v", elapsed)
	}
}

func TestShaperTailDrop(t *testing.T) {
	// Queue capacity for ~2 packets; no drainer running, so it fills and drops.
	pkt := make([]byte, 1000)
	cost := 1000 + shaperOverhead
	s := newShaper(1000, 0, 2*cost, nil, func(*peerSession, []byte) {})
	// don't start run(): the queue stays full

	accepted := 0
	for i := 0; i < 6; i++ {
		if s.enqueue(nil, pkt) {
			accepted++
		}
	}
	if accepted != 2 {
		t.Fatalf("expected exactly 2 packets to fit the queue, got %d", accepted)
	}
}
