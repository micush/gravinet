package dhcrelay

import (
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

// pkt builds a minimal well-formed DHCP datagram.
func pkt(op byte) []byte {
	b := make([]byte, minLen)
	b[offOp] = op
	copy(b[headerLen:], magicCookie[:])
	return b
}

func setAddr(b []byte, off int, s string) {
	a := netip.MustParseAddr(s).As4()
	copy(b[off:off+4], a[:])
}

// setChaddr writes an Ethernet client hardware address, which is what a reply
// carrying a not-yet-held address has to be framed to.
func setChaddr(b []byte, mac string) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		panic(err)
	}
	b[offHtype] = htypeEthernet
	b[offHlen] = ethAddrLen
	copy(b[offChaddr:offChaddr+ethAddrLen], hw)
}

// directRecorder stands in for the packet socket, keeping what would have gone
// on the link so the direct path can be asserted without CAP_NET_RAW.
type directRecorder struct {
	macs  []string
	dsts  []string
	srcs  []string
	calls int
}

func (d *directRecorder) sendDirect(mac net.HardwareAddr, srcIP, dstIP netip.Addr, _ []byte) error {
	d.calls++
	d.macs = append(d.macs, mac.String())
	d.srcs = append(d.srcs, srcIP.String())
	d.dsts = append(d.dsts, dstIP.String())
	return nil
}

func (d *directRecorder) Close() error { return nil }

var self = netip.MustParseAddr("10.1.1.1")

// A datagram that is not a DHCP message is dropped rather than forwarded. This
// socket receives every broadcast on the link, so malformed input is the
// normal case: forwarding it would make this relay a reflector pointed at
// somebody's DHCP server.
func TestParseRejectsNonDHCP(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":       {},
		"short":       make([]byte, headerLen),
		"no cookie":   append(make([]byte, headerLen), 0, 0, 0, 0),
		"bad op code": func() []byte { p := pkt(7); return p }(),
	} {
		if _, err := parse(b); err == nil {
			t.Errorf("%s: accepted as a DHCP message", name)
		}
	}
	if _, err := parse(pkt(opRequest)); err != nil {
		t.Errorf("a well-formed request was rejected: %v", err)
	}
}

// The giaddr rule from RFC 1542 §4.1.1, which is the single most load-bearing
// thing a relay does: the server picks which subnet to lease from by looking
// at giaddr, and addresses its reply back to it.
func TestPrepareRequestStampsGiaddrOnlyWhenUnset(t *testing.T) {
	// Zero giaddr: this relay claims it.
	b := pkt(opRequest)
	m, _ := parse(b)
	if err := prepareRequest(m, self, 0); err != nil {
		t.Fatalf("a fresh request was refused: %v", err)
	}
	if got := m.giaddr(); got != self {
		t.Errorf("giaddr = %s, want %s", got, self)
	}
	if m.hops() != 1 {
		t.Errorf("hops = %d, want 1", m.hops())
	}

	// Non-zero giaddr: another relay is already in the path and owns the
	// reply. Overwriting it would send the server's answer to this node
	// instead of to the relay the client can actually hear.
	other := netip.MustParseAddr("10.9.9.9")
	b = pkt(opRequest)
	setAddr(b, offGiaddr, other.String())
	m, _ = parse(b)
	if err := prepareRequest(m, self, 0); err != nil {
		t.Fatalf("a twice-relayed request was refused: %v", err)
	}
	if got := m.giaddr(); got != other {
		t.Errorf("giaddr was overwritten: got %s, want %s left alone", got, other)
	}
	if m.hops() != 1 {
		t.Errorf("hops = %d, want 1", m.hops())
	}
}

// Two independent loop bounds. The hop limit is the one the RFC specifies; the
// giaddr check catches a packet this relay already stamped coming back on a
// link it listens on, which is the loop the hop count only bounds rather than
// prevents.
func TestPrepareRequestBoundsLoops(t *testing.T) {
	b := pkt(opRequest)
	b[offHops] = defaultMaxHops
	m, _ := parse(b)
	if err := prepareRequest(m, self, 0); err == nil {
		t.Error("a request at the default hop limit was forwarded")
	}

	b = pkt(opRequest)
	b[offHops] = 2
	m, _ = parse(b)
	if err := prepareRequest(m, self, 3); err != nil {
		t.Errorf("a request below an explicit limit was refused: %v", err)
	}

	b = pkt(opRequest)
	setAddr(b, offGiaddr, self.String())
	m, _ = parse(b)
	if err := prepareRequest(m, self, 0); err == nil {
		t.Error("a request this relay had already stamped was forwarded again")
	}

	// A reply is not a request and must not be relayed as one.
	m, _ = parse(pkt(opReply))
	if err := prepareRequest(m, self, 0); err == nil {
		t.Error("a server reply was forwarded as a client request")
	}
}

// Where a reply goes on the client link. A client with no address is not
// listening for unicast, so its reply has to be broadcast — and to the limited
// broadcast address, since it has no netmask yet to derive a subnet broadcast
// from.
func TestReplyTarget(t *testing.T) {
	bcast := netip.MustParseAddr("255.255.255.255")
	ciaddr := netip.MustParseAddr("10.1.1.50")

	// No ciaddr yet: broadcast even without the B flag.
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	m, _ := parse(b)
	rp, err := replyTarget(m, self)
	if err != nil || rp.to != bcast {
		t.Errorf("addressless client: got %s (%v), want %s", rp.to, err, bcast)
	}

	// Has an address: unicast to it.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offCiaddr, ciaddr.String())
	m, _ = parse(b)
	rp, err = replyTarget(m, self)
	if err != nil || rp.to != ciaddr {
		t.Errorf("addressed client: got %s (%v), want %s", rp.to, err, ciaddr)
	}
	// A client that already holds the address answers ARP for it, so this
	// one goes through the socket like any other unicast.
	if rp.direct {
		t.Error("a reply to an address the client already holds was marked for direct delivery")
	}

	// Has an address but asked for broadcast anyway.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offCiaddr, ciaddr.String())
	b[offFlags] = 0x80
	m, _ = parse(b)
	rp, err = replyTarget(m, self)
	if err != nil || rp.to != bcast {
		t.Errorf("B flag set: got %s (%v), want %s", rp.to, err, bcast)
	}
}

// A reply belonging to a different relay in the path must not be delivered:
// doing so would put two copies of the same answer on the link.
func TestReplyTargetRefusesAnotherRelaysReply(t *testing.T) {
	b := pkt(opReply)
	setAddr(b, offGiaddr, "10.9.9.9")
	m, _ := parse(b)
	if _, err := replyTarget(m, self); err == nil {
		t.Error("delivered a reply addressed to a different relay")
	}
	// And a request is not a reply.
	m, _ = parse(pkt(opRequest))
	if _, err := replyTarget(m, self); err == nil {
		t.Error("treated a client request as a server reply")
	}
}

// Start refuses a configuration it cannot act on rather than sitting there
// looking healthy. Neither of these binds a socket, so the test does not need
// port 67 or privileges.
func TestStartRefusesEmptyConfig(t *testing.T) {
	if _, err := Start(Config{Links: []Link{{Iface: "eth0"}}}, nil); err == nil {
		t.Error("started with no upstream servers")
	}
	if _, err := Start(Config{}, nil); err == nil {
		t.Error("started with no client-facing interfaces")
	}
	// One link with nowhere to forward to fails the whole start rather than
	// being skipped: that is a configuration error, not a host condition like
	// an interface that cannot be bound.
	cfg := Config{Links: []Link{
		{Iface: "eth0", Servers: []netip.Addr{netip.MustParseAddr("10.0.0.5")}},
		{Iface: "eth1"},
	}}
	if _, err := Start(cfg, nil); err == nil {
		t.Error("started with a link that had no upstream servers")
	}
}

// Stop is called on every apply, including one that changes nothing, and on a
// relay that never started.
func TestStopIsSafeOnNilAndTwice(t *testing.T) {
	var r *Relay
	r.Stop()
	r2 := &Relay{done: make(chan struct{})}
	r2.Stop()
	r2.Stop()
}

// A DHCPOFFER is the case that matters and the case the tests above missed.
// The client is in SELECTING, so ciaddr is zero and the address being offered
// is in yiaddr. A relay that reads only ciaddr has nothing to unicast at
// exactly when it matters, and falls back to broadcasting every reply.
func TestReplyTargetUnicastsOfferToYiaddr(t *testing.T) {
	yiaddr := netip.MustParseAddr("10.4.4.37")
	ciaddr := netip.MustParseAddr("10.4.4.37")

	// Offer: yiaddr set, ciaddr zero, B flag clear.
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, yiaddr.String())
	m, _ := parse(b)
	rp, err := replyTarget(m, self)
	if err != nil {
		t.Fatalf("offer refused: %v", err)
	}
	if rp.to != yiaddr {
		t.Errorf("offer went to %s, want a unicast to %s", rp.to, yiaddr)
	}
	// And it must not go through a socket: the client does not hold this
	// address yet and cannot answer ARP for it. See TestOfferIsDeliveredToChaddr.
	if !rp.direct {
		t.Error("an offer was left to ordinary routing; it will be dropped in the ARP queue")
	}

	// An offer to a client that set the B flag is still broadcast.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, yiaddr.String())
	b[offFlags] = 0x80
	m, _ = parse(b)
	if rp, err = replyTarget(m, self); err != nil || rp.to != netip.MustParseAddr("255.255.255.255") {
		t.Errorf("B flag set: got %s (%v), want a broadcast", rp.to, err)
	}

	// Renewing: both are set to the same address, and either answer is the
	// same wire result. Pinned so the precedence is not accidental.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, yiaddr.String())
	setAddr(b, offCiaddr, ciaddr.String())
	m, _ = parse(b)
	if rp, err = replyTarget(m, self); err != nil || rp.to != yiaddr {
		t.Errorf("renewal: got %s (%v), want %s", rp.to, err, yiaddr)
	}
}

// recorder is a net.PacketConn that keeps what was written to it, so the
// direction a datagram left by can be asserted without binding anything.
type recorder struct {
	name string
	sent []net.Addr
}

func (c *recorder) WriteTo(b []byte, a net.Addr) (int, error) {
	c.sent = append(c.sent, a)
	return len(b), nil
}
func (c *recorder) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, io.EOF }
func (c *recorder) Close() error                           { return nil }
func (c *recorder) LocalAddr() net.Addr                    { return nil }
func (c *recorder) SetDeadline(time.Time) error            { return nil }
func (c *recorder) SetReadDeadline(time.Time) error        { return nil }
func (c *recorder) SetWriteDeadline(time.Time) error       { return nil }

// The socket a datagram leaves by is the whole fix. A request read on the LAN
// must go out the server-facing socket, and the reply must come back in on
// that same socket and leave by the client-facing one. Sending either from the
// socket it arrived on is what made this relay work only when the server was
// already on the client's link — the one topology needing no relay at all.
func TestHandleForwardsOnTheOppositeSocket(t *testing.T) {
	server := netip.MustParseAddr("10.1.1.1")
	newLink := func() (*link, *recorder, *recorder, *directRecorder) {
		cl, sv := &recorder{name: "client"}, &recorder{name: "server"}
		dr := &directRecorder{}
		return &link{
			cfg:    Link{Iface: "eth1", Servers: []netip.Addr{server}},
			self:   self,
			client: cl,
			server: sv,
			direct: dr,
		}, cl, sv, dr
	}
	r := &Relay{log: func(string, ...any) {}}

	// A client's request leaves by the server-facing socket, aimed at the
	// upstream server on port 67.
	lk, cl, sv, _ := newLink()
	r.handle(lk, fromClient, pkt(opRequest))
	if len(cl.sent) != 0 {
		t.Errorf("request went back out the client-facing socket, to %v", cl.sent)
	}
	if len(sv.sent) != 1 {
		t.Fatalf("request out the server-facing socket: %d datagrams, want 1", len(sv.sent))
	}
	if got, want := sv.sent[0].String(), "10.1.1.1:67"; got != want {
		t.Errorf("request went to %s, want %s", got, want)
	}

	// The server's reply arrives on the server-facing socket and never goes
	// back out by it. It carries an address the client does not hold yet, so
	// it leaves on the client link by the packet socket rather than the UDP
	// one — see TestOfferIsFramedToChaddr for why that is the only path that
	// reaches the client.
	lk, cl, sv, dr := newLink()
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, "10.4.4.37")
	setChaddr(b, "0c:e5:21:3f:00:00")
	r.handle(lk, fromServer, b)
	if len(sv.sent) != 0 {
		t.Errorf("reply went back out the server-facing socket, to %v", sv.sent)
	}
	if len(cl.sent) != 0 {
		t.Errorf("reply went out the UDP client socket, to %v — it would be dropped resolving an address the client does not hold", cl.sent)
	}
	if dr.calls != 1 {
		t.Fatalf("reply out the packet socket: %d frames, want 1", dr.calls)
	}
	if got, want := dr.dsts[0], "10.4.4.37"; got != want {
		t.Errorf("reply went to %s, want %s", got, want)
	}
}

// Traffic arriving on the wrong socket for its op code is not this relay's
// business. A reply on the LAN is another server answering the client
// directly; a request on the server-facing socket is somebody else's relay
// pointed at this address. Forwarding either makes this a reflector.
func TestHandleIgnoresWrongDirection(t *testing.T) {
	r := &Relay{log: func(string, ...any) {}}
	for name, tc := range map[string]struct {
		s side
		b []byte
	}{
		"reply on the client-facing socket":   {fromClient, pkt(opReply)},
		"request on the server-facing socket": {fromServer, pkt(opRequest)},
	} {
		cl, sv := &recorder{}, &recorder{}
		lk := &link{
			cfg:    Link{Iface: "eth1", Servers: []netip.Addr{netip.MustParseAddr("10.1.1.1")}},
			self:   self,
			client: cl,
			server: sv,
		}
		b := tc.b
		setAddr(b, offGiaddr, self.String())
		r.handle(lk, tc.s, b)
		if len(cl.sent)+len(sv.sent) != 0 {
			t.Errorf("%s: forwarded %v / %v", name, cl.sent, sv.sent)
		}
	}
}
