package transport

import (
	"net/netip"
	"testing"
)

// Dual.Send chooses its underlay by asking "is there a TLS connection to this
// address" — an address check, on an address two peers can legitimately share.
//
// TCP/65432 forwarded to one host and UDP/65432 to another, on one external
// IP, is two independent NAT forward rules and an entirely ordinary setup. The
// UDP peer's session is UDP; it has no TCP path at all. But the TCP peer's TLS
// connection sits at the same host:port, so HasConn is true for the UDP peer's
// endpoint too, and its traffic is diverted onto the other peer's TCP socket.
// Observed as tx climbing with rx flat at zero, and as the UDP peer being
// reported with transport "tcp", which it never was.
//
// The transport cannot resolve this from the address, because the address is
// genuinely the same. The session knows which underlay it came up on; it has
// to say so.

func TestSendViaUDPIgnoresAnotherPeersTLSConn(t *testing.T) {
	tls := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("174.64.247.165:65432")

	// The TCP peer's connection, at the shared address.
	c, peer := newPipeConn("174.64.247.165:65432")
	defer peer.Close()
	tls.register(ap, c)

	udp := newCountingUDP(t)
	d := Dual{UDP: udp.tr, TLS: tls}

	// A session that came up over UDP must go over UDP, regardless of what is
	// connected at that address on behalf of some other peer.
	if err := d.SendVia(ap, []byte("x"), ProtoUDP); err != nil {
		t.Fatalf("SendVia(udp): %v", err)
	}
	if got := udp.count(); got != 1 {
		t.Fatalf("udp sends = %d, want 1 — the datagram went to the other peer's TCP connection", got)
	}
	if c.isClosed() {
		t.Error("the other peer's connection was disturbed")
	}
}

// The TCP direction must still work: a session that came up over TCP uses the
// TLS connection and does not silently fall through to UDP, which for a
// UDP-hostile path would mean the traffic never arrives.
func TestSendViaTCPUsesTLS(t *testing.T) {
	tls := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("198.51.100.7:65432")
	c, peer := newPipeConn("198.51.100.7:65432")
	defer peer.Close()
	go func() { // drain, so the framed write completes
		buf := make([]byte, 4096)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()
	tls.register(ap, c)

	udp := newCountingUDP(t)
	d := Dual{UDP: udp.tr, TLS: tls}

	if err := d.SendVia(ap, []byte("x"), ProtoTCP); err != nil {
		t.Fatalf("SendVia(tcp): %v", err)
	}
	if got := udp.count(); got != 0 {
		t.Fatalf("udp sends = %d, want 0 — a TCP session must not leak onto UDP", got)
	}
}

// With UDP off entirely, a UDP-preferring send has nowhere to go and must say
// so rather than silently using another peer's TLS connection.
func TestSendViaUDPWithNoUDPTransport(t *testing.T) {
	tls := &TLSTransport{conns: map[netip.AddrPort][]*tlsConn{}}
	ap := netip.MustParseAddrPort("174.64.247.165:65432")
	c, peer := newPipeConn("174.64.247.165:65432")
	defer peer.Close()
	tls.register(ap, c)

	d := Dual{UDP: nil, TLS: tls}
	if err := d.SendVia(ap, []byte("x"), ProtoUDP); err == nil {
		t.Fatal("want an error; silently using the TLS conn is how the wrong peer got the traffic")
	}
}

// countingUDP is a real UDP transport bound to loopback, used only to count
// datagrams that actually left via UDP.
type countingUDP struct {
	tr *Transport
}

func newCountingUDP(t *testing.T) *countingUDP {
	t.Helper()
	tr, err := Open(Options{PrimaryPort: 0, EnableV4: true, Handler: func([]byte, netip.AddrPort, Family) {}})
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return &countingUDP{tr: tr}
}

func (c *countingUDP) count() uint64 { _, tx := c.tr.Stats(); return tx }
