// Package dhcrelay is a DHCPv4 relay agent: it forwards client broadcasts on
// a LAN to one or more upstream DHCP servers, and forwards the replies back.
//
// In-tree rather than driven as a daemon, which is the opposite of how
// gravinet handles radvd, FRR and Kea. The reason is that a relay is small and
// completely specified — RFC 2131 §4.3.5 and RFC 1542 §4.1 are four pages
// between them, and there is no state to keep — while every packaged relay is
// either end-of-life (ISC dhcrelay) or a second copy of a daemon this node is
// already running for something else (dnsmasq, which would also want to do DNS
// and router advertisements). Adding a dependency for four pages of forwarding
// is the worse trade, and it would be this project's first module dependency.
//
// IPv4 only. A DHCPv6 relay is a different message shape — Relay-Forward and
// Relay-Reply wrap the client's message rather than amending it in place, and
// it is multicast to ff02::1:2 rather than broadcast. That is a real gap and
// it is named as one; nothing here forecloses adding it beside this.
package dhcrelay

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// BOOTP/DHCP wire constants. Named rather than inlined because the two ports
// are easy to transpose and the consequence of doing so is a relay that
// listens where nothing is talking.
const (
	// ServerPort is where servers and relays listen; ClientPort is where
	// clients do. A relay uses ServerPort in both directions: upstream it is
	// acting as a client of the server, and RFC 1542 §4.1 has it send from
	// and to 67 so replies come back to the relay rather than to the client.
	ServerPort = 67
	ClientPort = 68

	// opRequest and opReply are the BOOTP op codes. The op code is what
	// decides direction here — a relay's two jobs are distinguished by it and
	// by nothing else.
	opRequest = 1
	opReply   = 2

	// headerLen is the fixed BOOTP header through the end of `file`, before
	// the options area. A datagram shorter than this is not a DHCP message.
	headerLen = 236

	// minLen is the shortest datagram worth forwarding: the header plus the
	// four-byte magic cookie. Anything shorter cannot carry a message type
	// and is not a packet a server would answer.
	minLen = headerLen + 4

	// maxLen bounds what will be read and forwarded. RFC 2131 puts the
	// minimum an implementation must accept at 576; 1500 covers anything a
	// client will actually send on an Ethernet LAN without fragmenting, and
	// caps what a hostile sender can make this allocate.
	maxLen = 1500

	// defaultMaxHops is RFC 1542 §4.1.1's limit. A packet arriving with this
	// many relays already in front of it is dropped rather than forwarded,
	// which is what bounds a relay loop.
	defaultMaxHops = 4
)

// Header field offsets. The BOOTP header is fixed-layout, so these are the
// whole parser: there is no framing to walk and no length prefix to trust.
const (
	offOp     = 0
	offHops   = 3
	offFlags  = 10
	offCiaddr = 12
	offGiaddr = 24
)

// flagBroadcast is the B flag in the `flags` field. A client that cannot
// receive a unicast reply before it has an address sets it, and the reply must
// then be broadcast.
const flagBroadcast = 0x8000

// magicCookie is the four bytes that begin the options area (RFC 2132 §2).
// Checked rather than assumed: without it the bytes after the header are not
// DHCP options, and forwarding them upstream is forwarding noise to a server.
var magicCookie = [4]byte{99, 130, 83, 99}

// msg is a DHCP datagram being relayed. A view over the caller's buffer, not a
// copy: the two mutations a relay makes — giaddr and hops — are in-place edits
// to a fixed-offset header, and parsing the whole message into a struct to
// change two fields would be more code with more to get wrong.
type msg struct{ b []byte }

// parse validates that a datagram is a DHCP message worth relaying at all.
//
// Every check here is a reason to drop rather than a reason to error: a relay
// listens on a broadcast port on a LAN, so malformed input is the expected
// case, not an exceptional one, and the answer is always silence.
func parse(b []byte) (msg, error) {
	if len(b) < minLen {
		return msg{}, fmt.Errorf("short datagram: %d bytes, need at least %d", len(b), minLen)
	}
	if op := b[offOp]; op != opRequest && op != opReply {
		return msg{}, fmt.Errorf("not a BOOTP message: op %d", op)
	}
	for i, c := range magicCookie {
		if b[headerLen+i] != c {
			return msg{}, fmt.Errorf("no DHCP magic cookie")
		}
	}
	return msg{b}, nil
}

func (m msg) op() byte   { return m.b[offOp] }
func (m msg) hops() byte { return m.b[offHops] }

func (m msg) flags() uint16 { return binary.BigEndian.Uint16(m.b[offFlags:]) }

// broadcastWanted reports whether a reply must go to the broadcast address:
// either the client asked for it, or it has no address yet to unicast to.
func (m msg) broadcastWanted() bool {
	return m.flags()&flagBroadcast != 0 || m.ciaddr() == netip.AddrFrom4([4]byte{})
}

func (m msg) ciaddr() netip.Addr { return addr4(m.b[offCiaddr:]) }
func (m msg) giaddr() netip.Addr { return addr4(m.b[offGiaddr:]) }

func addr4(b []byte) netip.Addr {
	var a [4]byte
	copy(a[:], b[:4])
	return netip.AddrFrom4(a)
}

// setGiaddr stamps the relay's own address on the message. This is the single
// most load-bearing thing a relay does: the server picks the subnet to lease
// from by looking at giaddr, and addresses its reply back to it.
func (m msg) setGiaddr(a netip.Addr) {
	v := a.As4()
	copy(m.b[offGiaddr:offGiaddr+4], v[:])
}

// bumpHops increments the relay count, which is what makes a loop terminate.
func (m msg) bumpHops() { m.b[offHops]++ }

// prepareRequest readies a client message for forwarding to a server, and
// reports whether it should be forwarded at all.
//
// The giaddr rule is the subtle one. RFC 1542 §4.1.1 says a relay sets giaddr
// only if it is currently zero: a non-zero giaddr means another relay is
// already in the path and owns the reply, and overwriting it would send the
// server's answer to this node instead of to the relay the client can hear.
func prepareRequest(m msg, self netip.Addr, maxHops int) error {
	if m.op() != opRequest {
		return fmt.Errorf("not a client request")
	}
	if maxHops <= 0 {
		maxHops = defaultMaxHops
	}
	if int(m.hops()) >= maxHops {
		return fmt.Errorf("hop count %d has reached the limit of %d", m.hops(), maxHops)
	}
	// A message this node already relayed, arriving back on a link this node
	// listens on. Forwarding it again is the loop the hop count only bounds
	// rather than prevents, and here it can be recognised outright.
	if m.giaddr() == self {
		return fmt.Errorf("giaddr is already this relay: %s", self)
	}
	m.bumpHops()
	if m.giaddr() == netip.AddrFrom4([4]byte{}) {
		m.setGiaddr(self)
	}
	return nil
}

// replyTarget decides where a server's reply goes on the client link, and
// reports whether this relay should deliver it at all.
//
// Returns the address to send to; the caller sends from ServerPort to
// ClientPort. A broadcast reply goes to the limited broadcast address rather
// than the subnet broadcast: the client has no address and no netmask yet, so
// 255.255.255.255 is the only one it is listening for.
func replyTarget(m msg, self netip.Addr) (netip.Addr, error) {
	if m.op() != opReply {
		return netip.Addr{}, fmt.Errorf("not a server reply")
	}
	// A reply whose giaddr is not this relay belongs to a different relay in
	// the path. Delivering it anyway would put two copies on the link.
	if m.giaddr() != self {
		return netip.Addr{}, fmt.Errorf("reply is for relay %s, not %s", m.giaddr(), self)
	}
	if m.broadcastWanted() {
		return netip.AddrFrom4([4]byte{255, 255, 255, 255}), nil
	}
	return m.ciaddr(), nil
}
