package transport

import (
	"net/netip"
	"sync"
	"testing"
)

func testAddr(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
}

// TestSendRingFIFO checks that datagrams come back out of a ring in the order
// they went in, which is what lets the flusher hand a batch to sendmmsg without
// reordering a peer's traffic.
func TestSendRingFIFO(t *testing.T) {
	r := newSendRing(8)
	for i := 0; i < 5; i++ {
		if !r.enqueue(testAddr(uint16(1000+i)), []byte{byte(i)}) {
			t.Fatalf("enqueue %d refused on an empty ring", i)
		}
	}
	start, n := r.claim(16)
	if n != 5 {
		t.Fatalf("claim returned %d slots, want 5", n)
	}
	for i := uint64(0); i < n; i++ {
		s := &r.slots[(start+i)&r.mask]
		if s.n != 1 || s.buf[0] != byte(i) {
			t.Fatalf("slot %d holds %v, want payload %d", i, s.buf[:s.n], i)
		}
		if got, want := s.addr.Port(), uint16(1000+i); got != want {
			t.Fatalf("slot %d addr port %d, want %d", i, got, want)
		}
	}
	r.release(n)
	if _, n := r.claim(16); n != 0 {
		t.Fatalf("ring still reports %d slots after release", n)
	}
}

// TestSendRingCopiesPayload is the property the whole design rests on: Send's
// callers (mesh.sealAndSend) reuse their buffer the instant Send returns, so a
// queued datagram must not alias it.
func TestSendRingCopiesPayload(t *testing.T) {
	r := newSendRing(4)
	caller := []byte("original-payload")
	if !r.enqueue(testAddr(1), caller) {
		t.Fatal("enqueue refused")
	}
	for i := range caller { // caller recycles its buffer immediately
		caller[i] = 'X'
	}
	start, n := r.claim(4)
	if n != 1 {
		t.Fatalf("claim returned %d, want 1", n)
	}
	if got := string(r.slots[start&r.mask].buf); got != "original-payload" {
		t.Fatalf("queued payload was aliased: got %q", got)
	}
}

// TestSendRingFullFallsBack proves the ring-full case reports failure rather
// than blocking or dropping — Send then takes the direct per-packet write, so
// the worst case is exactly the unbatched behaviour.
func TestSendRingFullFallsBack(t *testing.T) {
	const size = 4
	r := newSendRing(size)
	for i := 0; i < size; i++ {
		if !r.enqueue(testAddr(uint16(i)), []byte("x")) {
			t.Fatalf("enqueue %d refused before the ring was full", i)
		}
	}
	if r.enqueue(testAddr(99), []byte("overflow")) {
		t.Fatal("enqueue succeeded on a full ring")
	}
	if got := r.FullDrops(); got != 1 {
		t.Fatalf("FullDrops = %d, want 1", got)
	}
	// Draining frees capacity again.
	_, n := r.claim(size)
	r.release(n)
	if !r.enqueue(testAddr(99), []byte("after-drain")) {
		t.Fatal("enqueue refused after the ring drained")
	}
}

// TestSendRingClaimStopsAtUncommitted checks the FIFO guarantee under a
// concurrent producer: a slot that has been claimed but not yet filled must
// block the flusher from reaching past it, or datagrams would be transmitted
// out of order (and slot 0 would be sent holding stale contents).
func TestSendRingClaimStopsAtUncommitted(t *testing.T) {
	r := newSendRing(8)
	r.tail.Add(1) // simulate a producer mid-enqueue holding slot 0
	if !r.enqueue(testAddr(2), []byte("second")) {
		t.Fatal("enqueue refused")
	}
	if _, n := r.claim(8); n != 0 {
		t.Fatalf("claim returned %d slots past an uncommitted one, want 0", n)
	}
	// Once slot 0 commits, both become visible in order.
	r.slots[0] = sendSlot{buf: []byte("first"), addr: testAddr(1), n: 5}
	r.ready[0].Store(true)
	start, n := r.claim(8)
	if n != 2 {
		t.Fatalf("claim returned %d, want 2", n)
	}
	if got := string(r.slots[start&r.mask].buf[:r.slots[start&r.mask].n]); got != "first" {
		t.Fatalf("first slot = %q, want %q", got, "first")
	}
}

// TestSendRingEmptyPayloadRefused keeps zero-length datagrams off the batched
// path: the flusher takes &buf[0] to build an iovec, which would panic on an
// empty slice.
func TestSendRingEmptyPayloadRefused(t *testing.T) {
	r := newSendRing(4)
	if r.enqueue(testAddr(1), nil) {
		t.Fatal("enqueue accepted an empty payload")
	}
	if r.enqueue(testAddr(1), []byte{}) {
		t.Fatal("enqueue accepted a zero-length payload")
	}
}

// TestSendRingConcurrentProducers hammers the CAS claim from many goroutines
// (the real shape: every mesh send goroutine shares one ring) and checks that
// every accepted datagram is delivered exactly once, with none lost, duplicated,
// or corrupted. Run this with -race.
func TestSendRingConcurrentProducers(t *testing.T) {
	const (
		producers = 8
		perGo     = 500
		size      = 64
	)
	r := newSendRing(size)

	seen := make(map[string]int)
	var consumerWG sync.WaitGroup
	stop := make(chan struct{})
	consumerWG.Add(1)
	go func() { // single consumer, like the flusher
		defer consumerWG.Done()
		for {
			start, n := r.claim(16)
			if n == 0 {
				select {
				case <-stop:
					if _, n := r.claim(16); n == 0 {
						return
					}
				default:
				}
				continue
			}
			for i := uint64(0); i < n; i++ {
				s := &r.slots[(start+i)&r.mask]
				seen[string(s.buf[:s.n])]++
			}
			r.release(n)
		}
	}()

	var accepted sync.Map
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perGo; i++ {
				payload := []byte(string(rune('a'+p)) + "-" + itoa(i))
				if r.enqueue(testAddr(uint16(p)), payload) {
					accepted.Store(string(payload), true)
				}
				// A refusal is legitimate (ring full => direct path); it just
				// means this payload should not show up in the consumer.
			}
		}(p)
	}
	wg.Wait()
	close(stop)
	consumerWG.Wait()

	nAccepted := 0
	accepted.Range(func(k, _ any) bool {
		nAccepted++
		if got := seen[k.(string)]; got != 1 {
			t.Fatalf("payload %q delivered %d times, want exactly 1", k, got)
		}
		return true
	})
	if nAccepted == 0 {
		t.Fatal("no payloads were accepted at all")
	}
	if len(seen) != nAccepted {
		t.Fatalf("consumer saw %d distinct payloads, producers accepted %d", len(seen), nAccepted)
	}
	t.Logf("%d/%d enqueued through the ring, the rest would have taken the direct path",
		nAccepted, producers*perGo)
}

// itoa avoids pulling strconv into the test for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
