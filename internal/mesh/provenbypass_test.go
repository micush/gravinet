package mesh

import (
	"io"
	"net/netip"
	"sync"
	"testing"

	"gravinet/internal/logx"
	"gravinet/internal/tun"
)

// The loop guard detects the exact condition a bypass route exists to undo: a
// route on this host is steering underlay traffic for a peer into the tunnel.
// Until v711 it only counted and warned, and the bypass came solely from
// acquireBypassRoute — which is gated on meshRouteCovers, i.e. on gravinet's
// own record (ns.osMetric) of the prefixes it believes it installed.
//
// When that record and the kernel's actual table disagree, the gate returns
// silently and the loop persists forever with nothing logged to say why. That
// is what was observed: five peers unreachable while the node was remote, each
// with an underlay address inside a redistributed prefix, each producing
// loop-guard drops, and no bypass route acquired for any of them.
//
// A looped datagram is the kernel's own answer to the same question and cannot
// be stale, so it is treated as proof and the precondition is skipped.

func TestLoopGuardInstallsBypassOnProof(t *testing.T) {
	var mu sync.Mutex
	var installed []netip.Prefix
	origGW := defaultGatewayFn
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("10.0.0.1"), IfIndex: 3}, nil
	}
	defer func() { defaultGatewayFn = origGW }()
	restore := addGatewayRouteFn
	addGatewayRouteFn = func(p netip.Prefix, gw netip.Addr, ifIndex int32, metric int) error {
		mu.Lock()
		installed = append(installed, p)
		mu.Unlock()
		return nil
	}
	defer func() { addGatewayRouteFn = restore }()
	if !gatewaySupported {
		t.Skip("no gateway backend on this platform; bypass is impossible here by design")
	}

	e := &Engine{log: logx.New(io.Discard, logx.LevelDebug), bypassRefs: map[netip.Addr]bypassRef{}}
	ns := &netState{physicalGW: map[int]physicalGWCache{}}
	ns.spec.ID = 0x99
	ns.spec.Dev = newFakeDev("mesh0")

	peer := netip.MustParseAddr("192.168.5.106") // a real one from the field case
	e.noteUnderlayLoop(ns, peer)

	mu.Lock()
	got := append([]netip.Prefix(nil), installed...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatal("a looped underlay datagram installed no bypass route: the kernel has demonstrated it is steering this peer's traffic into the tunnel, and without a host route out of it the peer stays unreachable indefinitely")
	}
	if got[0].Bits() != peer.BitLen() || got[0].Addr() != peer {
		t.Fatalf("installed %v, want a host route for %v exactly", got[0], peer)
	}
}

// The trigger is per-packet, so a storm must not become a storm of route
// installs.
func TestProvenBypassIsRateLimited(t *testing.T) {
	var mu sync.Mutex
	n := 0
	origGW := defaultGatewayFn
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("10.0.0.1"), IfIndex: 3}, nil
	}
	defer func() { defaultGatewayFn = origGW }()
	restore := addGatewayRouteFn
	addGatewayRouteFn = func(netip.Prefix, netip.Addr, int32, int) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	}
	defer func() { addGatewayRouteFn = restore }()
	if !gatewaySupported {
		t.Skip("no gateway backend on this platform")
	}

	e := &Engine{log: logx.New(io.Discard, logx.LevelDebug), bypassRefs: map[netip.Addr]bypassRef{}}
	ns := &netState{physicalGW: map[int]physicalGWCache{}}
	ns.spec.Dev = newFakeDev("mesh0")
	peer := netip.MustParseAddr("192.168.5.106")
	for i := 0; i < 50; i++ {
		e.noteUnderlayLoop(ns, peer)
	}
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 1 {
		t.Fatalf("50 looped datagrams produced %d route installs, want 1", got)
	}
}
