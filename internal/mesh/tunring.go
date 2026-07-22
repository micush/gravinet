package mesh

import "sync/atomic"

// tunRing is the write-side counterpart of transport.sendRing (see that
// file's doc comment for the full design rationale — this mirrors it
// exactly, minus the per-slot address transport needs and this doesn't): a
// bounded multi-producer/single-consumer queue handing decrypted overlay
// packets from every UDP read-worker goroutine calling deliverInner to one
// per-network flusher goroutine, which is the only thing allowed to drive
// that network's tun.Device GRO coalescer (see tungso.go).
//
// Same copy-at-enqueue reasoning as sendRing: deliverInner's ip argument
// aliases a decrypted-in-place receive buffer that's about to be reused for
// the next inbound datagram (see onData/readLoopBatched's buffer-reuse
// contract), so it must be copied into ring-owned storage before this
// returns, not handed off by reference.
type tunRing struct {
	slots []tunSlot
	ready []atomic.Bool
	mask  uint64

	head atomic.Uint64
	tail atomic.Uint64

	sig chan struct{}

	fullDrops atomic.Uint64
}

type tunSlot struct {
	buf []byte
	n   int
}

// newTunRing builds a ring with size slots. size must be a power of two.
func newTunRing(size int) *tunRing {
	return &tunRing{
		slots: make([]tunSlot, size),
		ready: make([]atomic.Bool, size),
		mask:  uint64(size - 1),
		sig:   make(chan struct{}, 1),
	}
}

// enqueue copies pkt into a claimed slot and wakes the flusher. Reports
// false when the ring is full (or pkt is empty), in which case the caller
// must write the packet itself on the direct path — a full ring is natural
// backpressure whose worst case is exactly the unbatched behaviour this
// exists to improve on, never a drop.
func (r *tunRing) enqueue(pkt []byte) bool {
	if len(pkt) == 0 {
		return false
	}
	size := uint64(len(r.slots))
	for {
		tail := r.tail.Load()
		if tail-r.head.Load() >= size {
			r.fullDrops.Add(1)
			return false
		}
		if !r.tail.CompareAndSwap(tail, tail+1) {
			continue
		}
		i := tail & r.mask
		s := &r.slots[i]
		if cap(s.buf) < len(pkt) {
			s.buf = make([]byte, len(pkt))
		}
		s.buf = s.buf[:len(pkt)]
		copy(s.buf, pkt)
		s.n = len(pkt)
		r.ready[i].Store(true)
		select {
		case r.sig <- struct{}{}:
		default:
		}
		return true
	}
}

// claim returns the position and count of the longest run of committed
// slots available to the flusher, up to max.
func (r *tunRing) claim(max int) (start, count uint64) {
	head := r.head.Load()
	tail := r.tail.Load()
	var n uint64
	for n < uint64(max) && head+n < tail && r.ready[(head+n)&r.mask].Load() {
		n++
	}
	return head, n
}

// release returns n drained slots to producers. Only the flusher calls it,
// and only after it is done reading those slots' buffers.
func (r *tunRing) release(n uint64) {
	head := r.head.Load()
	for i := uint64(0); i < n; i++ {
		r.ready[(head+i)&r.mask].Store(false)
	}
	r.head.Store(head + n)
}

// FullDrops reports how many enqueues fell back to the direct write path
// because the ring was full. Exposed for tests and diagnostics.
func (r *tunRing) FullDrops() uint64 { return r.fullDrops.Load() }
