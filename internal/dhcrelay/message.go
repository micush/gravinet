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
	"net"
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
	offHtype  = 1
	offHlen   = 2
	offHops   = 3
	offFlags  = 10
	offCiaddr = 12
	offYiaddr = 16
	offGiaddr = 24
	offChaddr = 28
)

// htypeEthernet is ARP hardware type 1, and ethAddrLen its address length.
// chaddr is a 16-byte field carrying an address of whatever type htype names,
// so both have to agree before the first six bytes can be read as a MAC.
const (
	htypeEthernet = 1
	ethAddrLen    = 6
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

// broadcastWanted reports whether the client asked for a broadcast reply.
//
// Only the B flag, deliberately. An earlier version also returned true when
// ciaddr was zero, which is true of every DHCPOFFER — a client in SELECTING
// has no address yet, so ciaddr is zero by definition. That made this return
// true for the entire initial exchange and reduced the relay to broadcasting
// every reply. The address such a client should be unicast at is in yiaddr,
// which is what replyTarget now reads.
func (m msg) broadcastWanted() bool {
	return m.flags()&flagBroadcast != 0
}

func (m msg) ciaddr() netip.Addr { return addr4(m.b[offCiaddr:]) }

// clientMAC returns the client's hardware address from chaddr, and whether it
// is one a frame can actually be addressed to.
//
// Three ways it is not. A htype/hlen pair that is not Ethernet means the first
// six bytes of chaddr are not a MAC and reading them as one would put the
// reply somewhere arbitrary. An all-zero chaddr is what a client sends when it
// is identifying itself by client-id alone, and is not an address at all. A
// group address — the multicast bit in the first octet, which covers broadcast
// — is never a single client's own address, and a reply sent to one would go
// to the whole link while claiming to be a unicast.
//
// The caller broadcasts instead when this returns false. That is the honest
// fallback: it reaches the client, at the cost of reaching everyone else too.
func (m msg) clientMAC() (net.HardwareAddr, bool) {
	if m.b[offHtype] != htypeEthernet || m.b[offHlen] != ethAddrLen {
		return nil, false
	}
	raw := m.b[offChaddr : offChaddr+ethAddrLen]
	if raw[0]&0x01 != 0 {
		return nil, false
	}
	zero := true
	for _, c := range raw {
		if c != 0 {
			zero = false
			break
		}
	}
	if zero {
		return nil, false
	}
	mac := make(net.HardwareAddr, ethAddrLen)
	copy(mac, raw)
	return mac, true
}

// yiaddr is the address the server is handing the client. Zero on anything
// that is not an offer or an acknowledgement.
func (m msg) yiaddr() netip.Addr { return addr4(m.b[offYiaddr:]) }
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

// reply is where a server's answer goes on the client link and how it has to
// be put there.
type reply struct {
	// to is the IPv4 destination.
	to netip.Addr
	// direct is set when to is an address the client does not hold yet, so
	// the frame must be addressed to its hardware address rather than
	// resolved. See frame.go for what goes wrong otherwise.
	direct bool
}

// replyTarget decides where a server's reply goes on the client link, and
// reports whether this relay should deliver it at all.
//
// The caller sends from ServerPort to ClientPort. RFC 1542 §4.1.2 sets the
// order: honour the B flag first, then unicast to yiaddr if the server
// assigned one, then ciaddr for a client that already had an address, and
// broadcast only when there is nothing else to aim at.
//
// yiaddr before ciaddr is the part worth stating. Through the whole initial
// exchange the client has no address, so ciaddr is zero and yiaddr holds the
// address being offered; a relay that consults only ciaddr has nothing to
// unicast at exactly when it matters. ciaddr still comes second because a
// client RENEWING already holds its address and puts it there, leaving yiaddr
// as a confirmation of the same value.
//
// Which is exactly why yiaddr is marked direct and ciaddr is not, and why
// that distinction is the whole point of this returning a struct. Reaching an
// address the client already holds is ordinary IP: it answers ARP for it, and
// the kernel's UDP stack does the rest. Reaching an address the server has
// only just chosen is not — nobody on the link answers for it yet. Both are
// "unicast to a client on this link" and only one of them can go through a
// socket.
//
// An ACK sets both fields, so a REBINDING client that does hold its address
// still takes the direct path. That is not a mistake: addressing a frame to
// the hardware address a client just wrote into its own request is correct
// whether or not it also holds the IP, and having one path for every assigned
// address is worth more than saving a resolution that would have succeeded.
//
// A broadcast reply goes to the limited broadcast address rather than the
// subnet broadcast: the client has no address and no netmask yet, so
// 255.255.255.255 is the only one it is listening for.
func replyTarget(m msg, self netip.Addr) (reply, error) {
	if m.op() != opReply {
		return reply{}, fmt.Errorf("not a server reply")
	}
	// A reply whose giaddr is not this relay belongs to a different relay in
	// the path. Delivering it anyway would put two copies on the link.
	if m.giaddr() != self {
		return reply{}, fmt.Errorf("reply is for relay %s, not %s", m.giaddr(), self)
	}
	if m.broadcastWanted() {
		return reply{to: bcast}, nil
	}
	if y := m.yiaddr(); y != netip.AddrFrom4([4]byte{}) {
		return reply{to: y, direct: true}, nil
	}
	if c := m.ciaddr(); c != netip.AddrFrom4([4]byte{}) {
		return reply{to: c}, nil
	}
	return reply{to: bcast}, nil
}

// bcast is the limited broadcast address, 255.255.255.255.
var bcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})
