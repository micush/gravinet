package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// A multi-port seed list exists to *find* a port that works, not to hold a
// connection on all of them. Until v710, primeTCPSeeds skipped a seed only on
// an exact-address match against an existing fallback, so every other port of
// the same host was re-dialled on every tick, forever.
//
// Measured in the field: 29,228 failed connects in twenty minutes — a steady
// 24 per second — against a host that was already a connected peer, twelve of
// its thirteen ports returning "connection refused" each time. It is the same
// shape as the seed churn fixed in v701, on the TCP path instead of UDP: there
// too the skip test was an exact-address match with no notion of "this host is
// already reached".

// refuseAllBut is the field condition the plain fake cannot express: a host
// where exactly one port answers and the rest return "connection refused".
// With every port succeeding, the pre-v710 code also stops dialling, so a test
// built on the plain fake passes either way and proves nothing.
type refuseAllBut struct {
	*fakeFallback
	open netip.AddrPort
}

func (r *refuseAllBut) DialTCP(to netip.AddrPort) error {
	r.mu.Lock()
	r.dialed = append(r.dialed, to)
	if r.has == nil {
		r.has = map[netip.AddrPort]bool{}
	}
	ok := to == r.open
	if ok {
		r.has[to] = true
	}
	r.mu.Unlock()
	if !ok {
		return errRefused
	}
	return nil
}

var errRefused = errConnRefused{}

type errConnRefused struct{}

func (errConnRefused) Error() string { return "connect: connection refused" }

func TestPrimeTCPSeedsStopsAtTheFirstWorkingPortOnAHost(t *testing.T) {
	e, base, ns := fallbackEngine(t, 65432)

	host := netip.MustParseAddr("198.51.100.9")
	ports := []int{7, 11, 13, 15, 17, 19, 21, 23, 70, 79, 443, 513, 65432}
	seeds := make([]netip.AddrPort, 0, len(ports))
	for _, p := range ports {
		seeds = append(seeds, netip.AddrPortFrom(host, uint16(p)))
	}
	// Only :65432 answers, exactly as in the field.
	f := &refuseAllBut{fakeFallback: base, open: netip.AddrPortFrom(host, 65432)}
	e.Attach(f)

	ns.mu.Lock()
	ns.tcpSeeds = seeds
	ns.mu.Unlock()

	// First pass: nothing is up, so every port is a legitimate candidate.
	e.primeTCPSeeds(ns)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !f.HasTCP(f.open) {
		time.Sleep(10 * time.Millisecond)
	}
	first := len(f.dials())
	if !f.HasTCP(f.open) {
		t.Fatal("the one open port was never connected")
	}

	// Now the host is reached. Every further tick must be silent — the twelve
	// refusing ports are of no use to anyone.
	for i := 0; i < 5; i++ {
		e.primeTCPSeeds(ns)
		time.Sleep(20 * time.Millisecond)
	}

	if got := len(f.dials()); got != first {
		t.Fatalf("re-dialled %d time(s) on a host already reached (was %d after the first pass): in the field this ran at 24 refused connects per second, indefinitely, against a peer that was already connected", got-first, first)
	}
}

// The suppression is per host, not global: an unreached host must still be
// dialled while a different one is up. Without this the fix would trade a dial
// storm for an unreachable mesh.
func TestPrimeTCPSeedsStillDialsUnreachedHosts(t *testing.T) {
	e, f, ns := fallbackEngine(t, 65432)

	up := netip.MustParseAddrPort("198.51.100.9:65432")
	other := netip.MustParseAddrPort("203.0.113.7:65432")
	ns.mu.Lock()
	ns.tcpSeeds = []netip.AddrPort{up, other}
	ns.mu.Unlock()

	e.primeTCPSeeds(ns)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	seen := map[netip.Addr]bool{}
	for _, d := range f.dials() {
		seen[d.Addr()] = true
	}
	if !seen[other.Addr()] {
		t.Fatalf("a host with no connection was never dialled: %v", f.dials())
	}
}
