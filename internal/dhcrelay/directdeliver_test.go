package dhcrelay

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// The bug this file exists for.
//
// A DHCPOFFER is addressed to yiaddr, an address the client does not hold and
// will not answer ARP for. Sent through a UDP socket it is dropped in the
// neighbour queue: WriteTo has already returned success, so nothing is logged,
// no counter moves, and the client re-sends its DISCOVER on backoff forever.
// Observed as four relays retrying against a server that was answering every
// one of them.
//
// The reply has to be framed to the client's own chaddr instead, which is why
// replyTarget reports how to deliver rather than only where.
func TestOfferIsFramedToChaddr(t *testing.T) {
	const mac = "0c:e5:21:3f:00:00"
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, "10.1.1.10")
	setChaddr(b, mac)

	dr := &directRecorder{}
	cl, sv := &recorder{name: "client"}, &recorder{name: "server"}
	lk := &link{
		cfg:    Link{Iface: "eth1", Servers: []netip.Addr{netip.MustParseAddr("10.255.255.254")}},
		self:   self,
		client: cl,
		server: sv,
		direct: dr,
	}
	r := &Relay{log: func(string, ...any) {}}
	r.handle(lk, fromServer, b)

	if dr.calls != 1 {
		t.Fatalf("offer was not delivered directly: %d frames, want 1", dr.calls)
	}
	if got := dr.macs[0]; got != mac {
		t.Errorf("offer framed to %s, want the client's chaddr %s", got, mac)
	}
	if got, want := dr.dsts[0], "10.1.1.10"; got != want {
		t.Errorf("offer addressed to %s, want %s", got, want)
	}
	// The source has to be the giaddr the client's own request was stamped
	// with. A reply from any other address is one the client will not match
	// against its outstanding request.
	if got, want := dr.srcs[0], self.String(); got != want {
		t.Errorf("offer sourced from %s, want the relay address %s", got, want)
	}
	if len(cl.sent) != 0 {
		t.Errorf("the offer also went out the UDP socket, to %v", cl.sent)
	}
}

// Without a usable hardware address there is nothing to frame to, and a
// unicast would be the silent drop this whole change is about. Broadcasting
// reaches the client at the cost of reaching the rest of the link too, which
// is the right trade when the alternative is not reaching it at all.
func TestUndeliverableDirectReplyFallsBackToBroadcast(t *testing.T) {
	for name, prep := range map[string]func(b []byte){
		"no chaddr at all":    func(b []byte) {},
		"not an ethernet mac": func(b []byte) { setChaddr(b, "0c:e5:21:3f:00:00"); b[offHtype] = 6 },
		"group address":       func(b []byte) { setChaddr(b, "01:00:5e:00:00:01") },
	} {
		t.Run(name, func(t *testing.T) {
			b := pkt(opReply)
			setAddr(b, offGiaddr, self.String())
			setAddr(b, offYiaddr, "10.1.1.10")
			prep(b)

			dr := &directRecorder{}
			cl := &recorder{name: "client"}
			lk := &link{cfg: Link{Iface: "eth1"}, self: self, client: cl, server: &recorder{}, direct: dr}
			(&Relay{log: func(string, ...any) {}}).handle(lk, fromServer, b)

			if dr.calls != 0 {
				t.Errorf("framed a reply to an unusable hardware address")
			}
			if len(cl.sent) != 1 {
				t.Fatalf("reply out the client socket: %d datagrams, want 1", len(cl.sent))
			}
			if got, want := cl.sent[0].String(), "255.255.255.255:68"; got != want {
				t.Errorf("fallback went to %s, want %s — a unicast here is dropped in the ARP queue", got, want)
			}
		})
	}
}

// A link whose packet socket could not be opened still answers. Start logs the
// failure and carries the link rather than dropping it, because broadcasting
// every reply is degraded and not broken — unlike a missing UDP socket, which
// leaves a link that accepts requests and never answers them.
func TestLinkWithoutPacketSocketBroadcasts(t *testing.T) {
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offYiaddr, "10.1.1.10")
	setChaddr(b, "0c:e5:21:3f:00:00")

	cl := &recorder{name: "client"}
	lk := &link{cfg: Link{Iface: "eth1"}, self: self, client: cl, server: &recorder{}, direct: nil}
	(&Relay{log: func(string, ...any) {}}).handle(lk, fromServer, b)

	if len(cl.sent) != 1 {
		t.Fatalf("reply out the client socket: %d datagrams, want 1", len(cl.sent))
	}
	if got, want := cl.sent[0].String(), "255.255.255.255:68"; got != want {
		t.Errorf("reply went to %s, want %s", got, want)
	}
}

// A client that already holds its address answers ARP for it, so a renewal
// reply has no need of the packet socket. Pinned because the direct path is
// strictly more code and it should not quietly become the only one.
func TestReplyToHeldAddressUsesTheSocket(t *testing.T) {
	b := pkt(opReply)
	setAddr(b, offGiaddr, self.String())
	setAddr(b, offCiaddr, "10.1.1.50")
	setChaddr(b, "0c:e5:21:3f:00:00")

	dr := &directRecorder{}
	cl := &recorder{name: "client"}
	lk := &link{cfg: Link{Iface: "eth1"}, self: self, client: cl, server: &recorder{}, direct: dr}
	(&Relay{log: func(string, ...any) {}}).handle(lk, fromServer, b)

	if dr.calls != 0 {
		t.Error("a reply to an address the client already holds was framed directly")
	}
	if len(cl.sent) != 1 || cl.sent[0].String() != "10.1.1.50:68" {
		t.Errorf("renewal went to %v, want a unicast to 10.1.1.50:68", cl.sent)
	}
}

// The headers are built by hand, so they are checked by hand. A wrong checksum
// is discarded by the receiver with no error anywhere — the same class of
// silent failure as the bug above, which is reason enough to pin it.
func TestIP4UDPFraming(t *testing.T) {
	src := netip.MustParseAddr("10.1.1.1")
	dst := netip.MustParseAddr("10.1.1.10")
	payload := []byte("hello dhcp")
	f := ip4udp(src, dst, ServerPort, ClientPort, payload)

	if got, want := len(f), ip4HeaderLen+udpHeaderLen+len(payload); got != want {
		t.Fatalf("frame is %d bytes, want %d", got, want)
	}
	if f[0] != 0x45 {
		t.Errorf("version/IHL is %#x, want 0x45", f[0])
	}
	if f[9] != protoUDP {
		t.Errorf("protocol is %d, want %d", f[9], protoUDP)
	}
	if got, want := binary.BigEndian.Uint16(f[2:]), uint16(len(f)); got != want {
		t.Errorf("total length field says %d, frame is %d", got, want)
	}
	if got := netip.AddrFrom4([4]byte(f[12:16])); got != src {
		t.Errorf("source %s, want %s", got, src)
	}
	if got := netip.AddrFrom4([4]byte(f[16:20])); got != dst {
		t.Errorf("destination %s, want %s", got, dst)
	}

	// A correct internet checksum sums to zero over the block it covers,
	// which is how a receiver verifies it and how this can be checked
	// without reimplementing the arithmetic being tested.
	if got := foldChecksum(partialSum(f[:ip4HeaderLen], 0)); got != 0 {
		t.Errorf("IPv4 header checksum does not verify: residual %#04x", got)
	}

	udp := f[ip4HeaderLen:]
	if got, want := binary.BigEndian.Uint16(udp[0:]), uint16(ServerPort); got != want {
		t.Errorf("source port %d, want %d", got, want)
	}
	if got, want := binary.BigEndian.Uint16(udp[2:]), uint16(ClientPort); got != want {
		t.Errorf("destination port %d, want %d", got, want)
	}
	if got, want := binary.BigEndian.Uint16(udp[4:]), uint16(udpHeaderLen+len(payload)); got != want {
		t.Errorf("UDP length field says %d, want %d", got, want)
	}
	pseudo := make([]byte, 12)
	s4, d4 := src.As4(), dst.As4()
	copy(pseudo[0:4], s4[:])
	copy(pseudo[4:8], d4[:])
	pseudo[9] = protoUDP
	binary.BigEndian.PutUint16(pseudo[10:], uint16(len(udp)))
	if got := foldChecksum(partialSum(udp, partialSum(pseudo, 0))); got != 0 {
		t.Errorf("UDP checksum does not verify: residual %#04x", got)
	}
	// Never transmitted as zero: on the wire that means "not computed", which
	// is a different statement from "computed and came out zero".
	if binary.BigEndian.Uint16(udp[6:]) == 0 {
		t.Error("UDP checksum was transmitted as zero")
	}
}

// htons is one byte swap and impossible to eyeball in a sockaddr. A missing
// swap there gives a socket that opens cleanly and puts nothing on the wire.
func TestHtons(t *testing.T) {
	if got := htons(0x0800); got != 0x0008 {
		t.Errorf("htons(0x0800) = %#04x, want 0x0008", got)
	}
}
