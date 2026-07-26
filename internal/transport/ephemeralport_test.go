package transport

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
)

// The precondition behind probeEphemeralPort, demonstrated rather than assumed:
// SO_REUSEPORT sockets are permitted to share a port. Every worker socket a
// Transport opens sets it, so when the requested port is 0 the kernel is free
// to satisfy the request with a port another Transport already holds — and the
// two then split each other's inbound datagrams, which presents as a peer that
// won't handshake rather than as an error.
//
// This test does not reproduce that selection race (it is timing-dependent and
// does not reproduce on demand). It pins the mechanism that makes it possible,
// so if a future kernel or a change here made SO_REUSEPORT sharing impossible,
// the reason probeEphemeralPort exists would be visibly gone.
func TestReusePortSharingIsPermitted(t *testing.T) {
	if !reusePort {
		t.Skip("SO_REUSEPORT not used on this platform")
	}
	lc := net.ListenConfig{Control: control}
	a, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	defer a.Close()
	port := a.LocalAddr().(*net.UDPAddr).Port

	b, err := lc.ListenPacket(context.Background(), "udp4", a.LocalAddr().String())
	if err != nil {
		t.Fatalf("a second SO_REUSEPORT socket could not share port %d: %v — "+
			"if this is now rejected, revisit probeEphemeralPort's rationale", port, err)
	}
	b.Close()
}

// probeEphemeralPort must hand out a distinct port to every concurrent caller.
// It binds without SO_REUSEPORT precisely so the kernel cannot satisfy two
// callers with the same port, which is the property Open relies on.
func TestProbeEphemeralPortIsDistinctUnderConcurrency(t *testing.T) {
	const n = 64
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	// Hold every probed port open until all probes finish; releasing them as
	// we go would let the kernel legitimately reuse one and mask a collision.
	var held []net.PacketConn
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := probeEphemeralPort("127.0.0.1", true)
			if err != nil {
				t.Errorf("probe: %v", err)
				return
			}
			c, err := net.ListenPacket("udp4", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(p)).String())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				held = append(held, c)
			}
			if seen[p] {
				t.Errorf("port %d handed out twice", p)
			}
			seen[p] = true
		}()
	}
	wg.Wait()
	for _, c := range held {
		c.Close()
	}
	if len(seen) < n/2 {
		t.Fatalf("only %d of %d probes succeeded", len(seen), n)
	}
}
