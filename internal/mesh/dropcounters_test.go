package mesh

import (
	"errors"
	"io"
	"net/netip"
	"testing"

	"gravinet/internal/logx"
)

// The data path has four drops that were silent until v704: three policy
// decisions and one failure. What makes them worth counting is not that
// packets are lost — often that is correct — but that none of them are
// reachable by control traffic. ctrlPing/ctrlPong touch neither the firewall,
// the policer, the overlay device, nor the egress queue, so a peer losing
// every data packet to any of them still reports a healthy RTT, a climbing
// uptime and clean fragment counters. At a glance that is indistinguishable
// from a working peer, which is exactly the state these make visible.
//
// The assertion in each case is that the drop increments its own counter and
// no other, so a non-zero value names a cause rather than just reporting a
// loss.

// failWriteDev stands in for a full device queue or a reader falling behind.
type failWriteDev struct {
	*fakeDev
	err error
}

func (d *failWriteDev) Write(b []byte) (int, error) { return 0, d.err }

// testIPv4 builds a minimal well-formed IPv4 header with the given addresses.
func testIPv4(src, dst netip.Addr) []byte {
	p := make([]byte, 20)
	p[0] = 0x45 // version 4, IHL 5
	p[9] = 1    // ICMP
	copy(p[12:16], src.AsSlice())
	copy(p[16:20], dst.AsSlice())
	return p
}

func TestTunWriteFailureIsCountedAndAttributed(t *testing.T) {
	dev := &failWriteDev{fakeDev: newFakeDev("x"), err: errors.New("device queue full")}
	ns := &netState{}
	ns.spec.Dev = dev

	src := netip.MustParseAddr("192.168.203.246")
	ps := &peerSession{nodeID: "gn-debian", net: ns, overlay4: src}
	pkt := testIPv4(src, netip.MustParseAddr("192.168.203.111"))

	e := &Engine{log: logx.New(io.Discard, logx.LevelDebug)}
	e.deliverInner(ps, pkt, len(pkt))

	if got := ps.tunWriteDrop.Load(); got != 1 {
		t.Fatalf("tunWriteDrop = %d, want 1: a packet that clears every check and then fails on the device write leaves no other trace — not in gravinet, and not in the receiving host's kernel counters either, because it never reached IP input", got)
	}
	for _, c := range []struct {
		name string
		val  uint64
	}{
		{"fwInDrop", ps.fwInDrop.Load()},
		{"policeDrop", ps.policeDrop.Load()},
		{"spoofDrop", ps.spoofDrop.Load()},
		{"egressQDrop", ps.egressQDrop.Load()},
	} {
		if c.val != 0 {
			t.Errorf("%s = %d, want 0: a device-write failure must not be attributed to a policy decision", c.name, c.val)
		}
	}
}

// A successful delivery must leave every counter at zero — otherwise a
// non-zero value means nothing.
func TestSuccessfulDeliveryCountsNoDrops(t *testing.T) {
	dev := newFakeDev("ok")
	ns := &netState{}
	ns.spec.Dev = dev

	src := netip.MustParseAddr("192.168.203.246")
	ps := &peerSession{nodeID: "gn-debian", net: ns, overlay4: src}
	pkt := testIPv4(src, netip.MustParseAddr("192.168.203.111"))

	e := &Engine{log: logx.New(io.Discard, logx.LevelDebug)}
	e.deliverInner(ps, pkt, len(pkt))

	for _, c := range []struct {
		name string
		val  uint64
	}{
		{"tunWriteDrop", ps.tunWriteDrop.Load()},
		{"fwInDrop", ps.fwInDrop.Load()},
		{"policeDrop", ps.policeDrop.Load()},
		{"spoofDrop", ps.spoofDrop.Load()},
	} {
		if c.val != 0 {
			t.Errorf("%s = %d after a delivery that should have succeeded, want 0", c.name, c.val)
		}
	}
}

// The spoof check must stay distinguishable from the write failure: both drop
// the packet, but one is a rejected peer and the other is a broken device.
func TestSpoofedSourceIsCountedSeparately(t *testing.T) {
	dev := newFakeDev("ok")
	ns := &netState{}
	ns.spec.Dev = dev

	owned := netip.MustParseAddr("192.168.203.246")
	foreign := netip.MustParseAddr("192.168.203.5")
	ps := &peerSession{nodeID: "gn-debian", net: ns, overlay4: owned}
	other := &peerSession{nodeID: "gn-cush2", net: ns, overlay4: foreign}

	// sourceAllowedFrom rejects only a source that demonstrably belongs to a
	// *different* node, and fails open with no forwarding snapshot published
	// (an unknown address is allowed, deliberately). So the snapshot has to
	// exist and has to attribute the foreign address elsewhere for the check
	// to have anything to reject.
	ns.fwd.Store(&fwdSnap{
		routes4: map[netip.Addr]*peerSession{owned: ps, foreign: other},
		routes6: map[netip.Addr]*peerSession{},
		byNode:  map[string]*peerSession{"gn-debian": ps, "gn-cush2": other},
	})

	pkt := testIPv4(foreign, netip.MustParseAddr("192.168.203.111"))

	e := &Engine{log: logx.New(io.Discard, logx.LevelDebug)}
	e.deliverInner(ps, pkt, len(pkt))

	if got := ps.spoofDrop.Load(); got != 1 {
		t.Fatalf("spoofDrop = %d, want 1", got)
	}
	if got := ps.tunWriteDrop.Load(); got != 0 {
		t.Fatalf("tunWriteDrop = %d, want 0: a packet rejected before the device write must not be counted as a write failure", got)
	}
}
