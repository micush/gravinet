package transport

import (
	"net/netip"
	"sync/atomic"
)

// sendRing is the hand-off between Transport.Send (many producer goroutines)
// and one flusher goroutine that coalesces queued datagrams into a single
// sendmmsg syscall (see batch_linux.go). It is a bounded multi-producer /
// single-consumer queue.
//
// Why a copy at enqueue. Send is synchronous today and its callers — notably
// mesh.sealAndSend, which encrypts into a pooled buffer and returns that buffer
// to the pool the instant Send returns — reuse their payload buffer
// immediately. Batching makes the write asynchronous, so the payload has to be
// copied into ring-owned storage at enqueue or the flusher would transmit
// whatever the caller reused the buffer for next. The copy is a ~100ns memmove
// that buys back a ~1-2us syscall, and the kernel copies the payload on
// sendto() anyway, so this adds a copy to the userspace side only.
//
// Slot buffers grow on demand rather than being preallocated to
// protocol.MaxUDPPayload (9472). Preallocating would cost ringSize*9472 ≈
// 2.4 MB per socket — with one socket per worker per family that is tens of
// megabytes of permanently-resident memory on a many-core box, nearly all of
// it wasted, since real traffic sits near the path MTU (~1300 bytes). Growing
// on demand converges on what the link actually carries and still handles
// jumbo datagrams when they appear.
//
// Ordering: a ring is strictly FIFO, so datagrams leaving through one socket
// keep their relative order. Sends are spread across sockets by the existing
// txRR round-robin, so two datagrams to the same peer can still be reordered
// relative to each other when they land on different rings. That is the same
// kind of reordering the TUN worker pool already introduced (see tunLoopPooled)
// and it is safe for the same reasons: UDP promises no ordering, the replay
// window (64) absorbs small reorderings, and TCP inside the tunnel has its own
// sequence numbers.
type sendRing struct {
	slots []sendSlot
	ready []atomic.Bool // ready[i]: slot i holds a committed payload
	mask  uint64

	head atomic.Uint64 // consumer cursor: next slot to transmit
	tail atomic.Uint64 // producer cursor: next slot to claim

	// sig wakes the flusher. Capacity 1 with a non-blocking send: the token
	// means "there is work", not "there are N items", so a producer never
	// blocks and wakeups are never lost — the token is deposited after the
	// payload is committed, and the flusher re-drains after every wakeup.
	sig chan struct{}

	// fullDrops counts enqueue attempts refused because the ring was full.
	// These are not drops on the wire: Send falls through to the direct
	// per-packet write, which is exactly today's behaviour.
	fullDrops atomic.Uint64
}

type sendSlot struct {
	buf  []byte
	addr netip.AddrPort
	n    int
}

// newSendRing builds a ring with size slots. size must be a power of two.
func newSendRing(size int) *sendRing {
	return &sendRing{
		slots: make([]sendSlot, size),
		ready: make([]atomic.Bool, size),
		mask:  uint64(size - 1),
		sig:   make(chan struct{}, 1),
	}
}

// enqueue copies payload into a claimed slot and wakes the flusher. It reports
// false when the ring is full (or the payload is empty), in which case the
// caller must send the datagram itself on the direct path. It never blocks and
// never drops: a full ring is natural backpressure whose worst case is exactly
// the unbatched behaviour.
func (r *sendRing) enqueue(to netip.AddrPort, payload []byte) bool {
	if len(payload) == 0 {
		return false // nothing to send; the direct path handles the degenerate case
	}
	size := uint64(len(r.slots))
	for {
		tail := r.tail.Load()
		if tail-r.head.Load() >= size {
			r.fullDrops.Add(1)
			return false
		}
		if !r.tail.CompareAndSwap(tail, tail+1) {
			continue // another producer took this slot; retry with a fresh cursor
		}
		// This goroutine now exclusively owns slot `tail` until it is marked
		// ready: no other producer can claim it (the CAS handed it out once)
		// and the flusher will not look at it (ready is still false).
		i := tail & r.mask
		s := &r.slots[i]
		if cap(s.buf) < len(payload) {
			s.buf = make([]byte, len(payload))
		}
		s.buf = s.buf[:len(payload)]
		copy(s.buf, payload)
		s.addr = to
		s.n = len(payload)
		r.ready[i].Store(true) // publishes the payload to the flusher
		select {
		case r.sig <- struct{}{}:
		default: // a wakeup is already pending
		}
		return true
	}
}

// claim returns the position and count of the longest run of committed slots
// available to the flusher, up to max. It stops at the first slot that has been
// claimed by a producer but not yet committed, which is what preserves FIFO
// order; that slot is picked up on the next pass.
func (r *sendRing) claim(max int) (start, count uint64) {
	head := r.head.Load()
	tail := r.tail.Load()
	var n uint64
	for n < uint64(max) && head+n < tail && r.ready[(head+n)&r.mask].Load() {
		n++
	}
	return head, n
}

// release returns n transmitted slots to producers. Only the flusher calls it,
// and only after it is done reading those slots' buffers.
func (r *sendRing) release(n uint64) {
	head := r.head.Load()
	for i := uint64(0); i < n; i++ {
		r.ready[(head+i)&r.mask].Store(false)
	}
	r.head.Store(head + n) // frees the slots for reuse
}

// FullDrops reports how many sends fell back to the direct path because the
// ring was full. Exposed for tests and diagnostics.
func (r *sendRing) FullDrops() uint64 { return r.fullDrops.Load() }
