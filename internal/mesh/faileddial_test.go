package mesh

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

// alwaysRefuse fails every dial, which is the commonest real condition: an
// address that is configured, unreachable, and never going to answer.
type alwaysRefuse struct {
	*fakeTCP
}

func (a *alwaysRefuse) DialTCP(to netip.AddrPort) error {
	a.mu.Lock()
	a.dialed = append(a.dialed, to)
	a.mu.Unlock()
	return errors.New("connect: connection refused")
}

// v713 backed off TCP dials that connect and then fail to handshake. It left
// the commoner case — a dial that fails outright — with no cooldown at all, so
// an unreachable address was retried on every tick indefinitely. In the field
// that was 780 dials a minute against a single address.
func TestFailedDialBacksOff(t *testing.T) {
	e, base, ns := tcpEngine(t, 65432)
	f := &alwaysRefuse{fakeTCP: base}
	e.Attach(f)

	dead := netip.MustParseAddrPort("198.51.100.9:65432")
	ns.mu.Lock()
	ns.tcpSeeds = []netip.AddrPort{dead}
	ns.mu.Unlock()

	e.primeTCPSeeds(ns)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	first := len(f.dials())
	if first == 0 {
		t.Fatal("the address was never dialled at all")
	}
	if !ns.tcpInBackoff(dead) {
		t.Fatal("a refused dial left no cooldown: this address will be retried on every tick forever, which is exactly the 13-per-second storm this fixes")
	}

	for i := 0; i < 10; i++ {
		e.primeTCPSeeds(ns)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(f.dials()); got != first {
		t.Fatalf("%d further dials while in backoff, want 0", got-first)
	}
}

// The address-keyed reached set cannot see that a peer's v6 address belongs to
// a peer already connected over v4 — they are different netip.Addr values. In
// the field one connected peer's other address family drew 14,113 dials in a
// single window.
func TestAddressesOfAConnectedPeerAreNotDialled(t *testing.T) {
	e, base, ns := tcpEngine(t, 65432)
	f := &alwaysRefuse{fakeTCP: base}
	e.Attach(f)

	v4 := netip.MustParseAddrPort("66.179.240.44:65432")
	v6 := netip.MustParseAddrPort("[2607:f1c0:f00c:db01::1]:65432")

	// Connected to ionos2 over v4; its v6 address is a known seed of the same
	// peer and must not be dialled.
	ps := &peerSession{nodeID: "gn-ionos2", net: ns, endpoint: v4}
	ns.mu.Lock()
	ns.byNode = map[string]*peerSession{"gn-ionos2": ps}
	if ns.seedOwner == nil {
		ns.seedOwner = map[netip.AddrPort]string{}
	}
	ns.seedOwner[v6] = "gn-ionos2"
	ns.tcpSeeds = []netip.AddrPort{v6}
	ns.mu.Unlock()

	e.primeTCPSeeds(ns)
	time.Sleep(150 * time.Millisecond)

	for _, d := range f.dials() {
		if d == v6 {
			t.Fatal("dialled the IPv6 address of a peer already connected over IPv4: the reached set is keyed by address, so the v4 session gives its v6 address no credit and it is retried forever")
		}
	}
}
