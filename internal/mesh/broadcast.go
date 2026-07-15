package mesh

import (
	"encoding/binary"
	"math"
	"net/netip"
	"sync"
	"time"
)

// tokenBucket is a simple rate limiter for storm control. A non-positive rate
// means unlimited.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second; <=0 means unlimited
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(pps, burst int) *tokenBucket {
	tb := &tokenBucket{now: time.Now}
	if pps <= 0 {
		return tb // unlimited
	}
	tb.rate = float64(pps)
	if burst <= 0 {
		burst = pps
	}
	tb.burst = float64(burst)
	tb.tokens = tb.burst
	tb.last = tb.now()
	return tb
}

func (b *tokenBucket) allow() bool {
	return b.allowN(1)
}

// allowN consumes n tokens if available. A non-positive rate means unlimited.
func (b *tokenBucket) allowN(n float64) bool {
	if b.rate <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// waitN reports how long until n tokens would be available (without consuming).
// Intended for a single drainer goroutine pacing egress.
func (b *tokenBucket) waitN(n float64) time.Duration {
	if b.rate <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens >= n {
		return 0
	}
	return time.Duration((n - b.tokens) / b.rate * float64(time.Second))
}

func (b *tokenBucket) refillLocked() {
	now := b.now()
	b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
}

type dstKind int

const (
	kindUnicast dstKind = iota
	kindBroadcast
	kindMulticast
)

var v4Broadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// classify decides whether an overlay destination is unicast, broadcast, or
// multicast (the latter two are flooded to the mesh under storm control).
func (ns *netState) classify(dst netip.Addr) dstKind {
	if dst.Is4() {
		if dst == v4Broadcast {
			return kindBroadcast
		}
		a := dst.As4()
		if a[0] >= 224 && a[0] <= 239 { // 224.0.0.0/4
			return kindMulticast
		}
		ns.mu.RLock()
		sub := ns.subnet4
		ns.mu.RUnlock()
		if sub.IsValid() && dst == subnetBroadcast4(sub) {
			return kindBroadcast
		}
		return kindUnicast
	}
	if dst.As16()[0] == 0xff { // ff00::/8
		return kindMulticast
	}
	return kindUnicast
}

func subnetBroadcast4(p netip.Prefix) netip.Addr {
	p = p.Masked()
	netUint := binary.BigEndian.Uint32(asSlice4(p.Addr()))
	hostBits := 32 - p.Bits()
	var hostMask uint32
	if hostBits > 0 {
		hostMask = (uint32(1) << hostBits) - 1
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], netUint|hostMask)
	return netip.AddrFrom4(b)
}

// flood replicates an overlay packet to every connected peer (full mesh / relay
// reach). Receivers write it to their interface but do not re-flood, so there
// are no loops.
func (e *Engine) flood(ns *netState, pkt []byte) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		e.sendData(ps, pkt)
	}
}
