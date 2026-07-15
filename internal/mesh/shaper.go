package mesh

import (
	"sync"
	"time"

	"gravinet/internal/protocol"
)

// Bandwidth throttling.
//
// Egress is *shaped*: packets enter a bounded queue and a single drainer
// goroutine releases them paced to the configured up-rate (so we smooth our own
// outbound traffic rather than dropping it). Ingress is *policed* by a byte
// token bucket on the receive path (we can't slow a remote sender over UDP, so
// inbound that exceeds the down-rate is dropped — which, for TCP flows, signals
// the sender to back off).

// shaperOverhead approximates the per-packet wire cost added on top of the inner
// IP packet (frame byte + data header + GCM tag); used for byte accounting.
const shaperOverhead = 1 + protocol.DataHeaderLen + protocol.GCMOverhead

const defaultShaperQueueBytes = 4 << 20 // 4 MiB

type shapedPkt struct {
	ps  *peerSession
	pkt []byte
}

type shaper struct {
	mu         sync.Mutex
	classes    [][]shapedPkt // index 0 = highest priority
	classBytes []int
	classCap   int // per-class byte capacity

	cls    *classifier // nil = single class (no QoS)
	bucket *tokenBucket
	send   func(ps *peerSession, pkt []byte)

	signal chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

func newShaper(bytesPerSec, burstBytes, queueBytes int, cls *classifier, send func(*peerSession, []byte)) *shaper {
	// Burst must be at least one maximum-size packet or a large packet could
	// never accumulate enough tokens to pass.
	if burstBytes < protocol.MaxUDPPayload {
		burstBytes = protocol.MaxUDPPayload
	}
	if queueBytes <= 0 {
		queueBytes = defaultShaperQueueBytes
	}
	n := cls.numClasses()
	return &shaper{
		classes:    make([][]shapedPkt, n),
		classBytes: make([]int, n),
		classCap:   queueBytes,
		cls:        cls,
		bucket:     newTokenBucket(bytesPerSec, burstBytes),
		send:       send,
		signal:     make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// enqueue classifies the packet, marks it with that class's outbound DSCP
// value, and appends it to its priority class, returning false (tail-drop)
// when that class's queue is full.
func (s *shaper) enqueue(ps *peerSession, pkt []byte) bool {
	class := s.cls.classify(pkt)
	if class < 0 || class >= len(s.classes) {
		class = len(s.classes) - 1
	}
	s.cls.markDSCP(pkt, class)
	cost := len(pkt) + shaperOverhead
	s.mu.Lock()
	if s.classBytes[class]+cost > s.classCap {
		s.mu.Unlock()
		return false
	}
	s.classes[class] = append(s.classes[class], shapedPkt{ps, pkt})
	s.classBytes[class] += cost
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
	return true
}

// pickLocked returns the highest-priority non-empty class and its head packet.
func (s *shaper) pickLocked() (int, shapedPkt) {
	for i := range s.classes {
		if len(s.classes[i]) > 0 {
			return i, s.classes[i][0]
		}
	}
	return -1, shapedPkt{}
}

func (s *shaper) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		ci, head := s.pickLocked()
		s.mu.Unlock()
		if ci < 0 {
			select {
			case <-s.stop:
				return
			case <-s.signal:
				continue
			}
		}

		cost := float64(len(head.pkt) + shaperOverhead)
		if d := s.bucket.waitN(cost); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-s.stop:
				t.Stop()
				return
			case <-t.C:
			}
		}

		// Re-select after waiting: a higher-priority packet may have arrived.
		s.mu.Lock()
		ci, head = s.pickLocked()
		if ci < 0 {
			s.mu.Unlock()
			continue
		}
		cost = float64(len(head.pkt) + shaperOverhead)
		if !s.bucket.allowN(cost) {
			s.mu.Unlock()
			continue // tokens not ready yet; re-pace
		}
		s.classes[ci] = s.classes[ci][1:]
		s.classBytes[ci] -= int(cost)
		if len(s.classes[ci]) == 0 {
			s.classes[ci] = nil
		}
		s.mu.Unlock()
		s.send(head.ps, head.pkt)
	}
}

func (s *shaper) close() {
	close(s.stop)
	<-s.done
}
