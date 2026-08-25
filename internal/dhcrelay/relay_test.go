package dhcrelay

import (
	"net/netip"
	"testing"
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
	to, err := replyTarget(m, self)
	if err != nil || to != bcast {
		t.Errorf("addressless client: got %s (%v), want %s", to, err, bcast)
	}

	// Has an address: unicast to it.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offCiaddr, ciaddr.String())
	m, _ = parse(b)
	to, err = replyTarget(m, self)
	if err != nil || to != ciaddr {
		t.Errorf("addressed client: got %s (%v), want %s", to, err, ciaddr)
	}

	// Has an address but asked for broadcast anyway.
	b = pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offCiaddr, ciaddr.String())
	b[offFlags] = 0x80
	m, _ = parse(b)
	to, err = replyTarget(m, self)
	if err != nil || to != bcast {
		t.Errorf("B flag set: got %s (%v), want %s", to, err, bcast)
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
