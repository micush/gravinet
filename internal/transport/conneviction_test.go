package transport

import (
	"net"
	"net/netip"
	"testing"
)

// Two peers behind one NAT gateway, port-forwarded to the same external
// address, reach this node over TCP. One was dialed outbound, the other
// arrived inbound — genuinely distinct TCP connections with distinct 4-tuples,
// both legal at once, and identical remote host:port.
//
// conns is keyed by remote AddrPort alone, and register() closes any existing
// connection at that key. So the two peers evict each other in a loop: the
// outbound dial kills the inbound connection, the inbound peer redials and
// kills the outbound one, forever. Observed in the field at roughly one cycle
// every 12-18 seconds, with the affected peer showing tx climbing and rx flat
// at zero, and unreachable for management whenever the fan-out caught it
// mid-cycle.
//
// The eviction is deliberate and right for the case it was written for — a
// peer whose NAT rebound, reconnecting from the same address, where the old
// socket really is dead. It simply cannot tell that apart from a different
// peer at the same address.

// pipeConn is a net.Conn standing in for one side of a TCP connection, with a
// settable remote address so two of them can share one.
type pipeConn struct {
	net.Conn
	remote net.Addr
	closed chan struct{}
}

func newPipeConn(remote string) (*pipeConn, net.Conn) {
	a, b := net.Pipe()
	ra, _ := net.ResolveTCPAddr("tcp", remote)
	return &pipeConn{Conn: a, remote: ra, closed: make(chan struct{})}, b
}

func (c *pipeConn) RemoteAddr() net.Addr { return c.remote }

func (c *pipeConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return c.Conn.Close()
}

func (c *pipeConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// TestTwoPeersOneEndpointDoNotEvictEachOther is the failure, reduced. Two
// connections register at one remote host:port; neither may close the other.
func TestTwoPeersOneEndpointDoNotEvictEachOther(t *testing.T) {
	tr := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("174.64.247.165:65432")

	cush1, peer1 := newPipeConn("174.64.247.165:65432")
	defer peer1.Close()
	cush2, peer2 := newPipeConn("174.64.247.165:65432")
	defer peer2.Close()

	tr.register(ap, cush1) // mcfed dialed out to cush1
	if cush1.isClosed() {
		t.Fatal("setup: the first connection was closed on registration")
	}

	tr.register(ap, cush2) // cush2 dialed in, from the same NAT address

	if cush1.isClosed() {
		t.Error("registering cush2's connection closed cush1's — this is the eviction loop: each peer's connect kills the other's, forever")
	}
	if cush2.isClosed() {
		t.Error("cush2's own connection was closed on registration")
	}
}

// The behaviour the eviction exists for must survive: a peer reconnecting from
// the same address after its NAT rebound leaves a dead socket behind, and that
// one should be reaped. Re-registering the *same* raw connection is a no-op,
// and a replacement is only a replacement when the old one is actually gone.
func TestSameConnectionReRegisterIsNoop(t *testing.T) {
	tr := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("198.51.100.7:65432")

	c, peer := newPipeConn("198.51.100.7:65432")
	defer peer.Close()

	tr.register(ap, c)
	tr.register(ap, c)
	if c.isClosed() {
		t.Fatal("re-registering the same connection closed it")
	}
	if !tr.HasConn(ap) {
		t.Fatal("the connection is no longer registered")
	}
}

// Unregistering one of two connections at a shared address must not take the
// other with it, or a single peer disconnecting would strand its neighbour.
func TestUnregisterOneOfTwoAtOneEndpoint(t *testing.T) {
	tr := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("174.64.247.165:65432")

	a, peerA := newPipeConn("174.64.247.165:65432")
	defer peerA.Close()
	b, peerB := newPipeConn("174.64.247.165:65432")
	defer peerB.Close()

	tr.register(ap, a)
	tr.register(ap, b)
	tr.unregister(ap, a)

	if b.isClosed() {
		t.Error("unregistering one peer's connection closed the other's")
	}
	if !tr.HasConn(ap) {
		t.Error("the surviving connection is no longer reachable at that address")
	}
}
