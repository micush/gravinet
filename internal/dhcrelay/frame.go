package dhcrelay

import (
	"encoding/binary"
	"net/netip"
)

// Building an IPv4/UDP datagram by hand, because the reply to a client that
// has no address yet cannot be sent through the kernel's UDP stack.
//
// A DHCPOFFER is addressed to yiaddr — the address the server has just picked
// out for a client that does not hold it yet and will not answer ARP for it.
// Handing that to a UDP socket asks the kernel to resolve a neighbour that by
// definition cannot answer: the ARP request goes out, nothing comes back, and
// the datagram is dropped in the queue. Nothing fails loudly. WriteTo returned
// success long before, so the relay logs nothing, the client re-sends its
// DISCOVER on backoff forever, and every packet counter along the path says
// the exchange is fine.
//
// The way out is to stop asking for resolution at all. chaddr in the client's
// own request is its hardware address, so the frame can be addressed straight
// to it and put on the link with AF_PACKET — which means supplying the IP and
// UDP headers here, since nothing above this is doing it any more.
//
// Kept free of build tags so the header construction is testable on any host,
// including the checksums, which are the part worth testing and the part with
// no visible symptom when wrong.

const (
	ip4HeaderLen = 20
	udpHeaderLen = 8

	// ip4TTL for a reply that never leaves the link. Not 1: a TTL of one is
	// legal here and would work, but it is also what a traceroute probe
	// looks like, and some middleboxes treat a low TTL as suspicious. 64 is
	// the ordinary default and carries no such signal.
	ip4TTL = 64

	protoUDP = 17
)

// ip4udp wraps payload in an IPv4 and a UDP header and returns the datagram,
// starting at the IP header. The Ethernet header is not built here — with an
// AF_PACKET SOCK_DGRAM socket the kernel supplies it from the destination
// hardware address given at send time.
func ip4udp(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	total := ip4HeaderLen + udpHeaderLen + len(payload)
	b := make([]byte, total)

	b[0] = 0x45 // IPv4, five 32-bit words of header, no options
	b[1] = 0x00 // no DSCP or ECN marking
	binary.BigEndian.PutUint16(b[2:], uint16(total))
	// Identification stays zero. It exists to reassemble fragments, and this
	// datagram is never fragmented: a DHCP message is bounded by maxLen, well
	// inside any link's MTU.
	binary.BigEndian.PutUint16(b[4:], 0)
	binary.BigEndian.PutUint16(b[6:], 0) // no flags, no fragment offset
	b[8] = ip4TTL
	b[9] = protoUDP
	// Header checksum is computed over the header with this field zeroed,
	// which it already is.
	s4, d4 := src.As4(), dst.As4()
	copy(b[12:16], s4[:])
	copy(b[16:20], d4[:])
	binary.BigEndian.PutUint16(b[10:], onesComplement(b[:ip4HeaderLen]))

	u := b[ip4HeaderLen:]
	binary.BigEndian.PutUint16(u[0:], sport)
	binary.BigEndian.PutUint16(u[2:], dport)
	binary.BigEndian.PutUint16(u[4:], uint16(udpHeaderLen+len(payload)))
	copy(u[udpHeaderLen:], payload)
	binary.BigEndian.PutUint16(u[6:], udpChecksum(s4, d4, u))

	return b
}

// udpChecksum computes the UDP checksum over the pseudo-header and the
// datagram, per RFC 768.
//
// Computed rather than left zero, which IPv4 permits and which would be less
// code. Two reasons not to take that option: a checksum of zero is indeed
// "not computed" to a conforming stack, but embedded DHCP clients in the wild
// are not uniformly conforming, and some NICs offload verification in a way
// that has historically been unkind to zero-checksum UDP. Neither is a risk
// worth taking to save six lines on a path that only carries a handful of
// packets per client.
func udpChecksum(src, dst [4]byte, udp []byte) uint16 {
	pseudo := make([]byte, 12)
	copy(pseudo[0:4], src[:])
	copy(pseudo[4:8], dst[:])
	pseudo[8] = 0
	pseudo[9] = protoUDP
	binary.BigEndian.PutUint16(pseudo[10:], uint16(len(udp)))

	sum := partialSum(pseudo, 0)
	sum = partialSum(udp, sum)
	c := foldChecksum(sum)
	// RFC 768: a computed checksum of zero is transmitted as all ones,
	// because zero on the wire is reserved to mean "no checksum here". The
	// two are not the same statement and one of them is checkable.
	if c == 0 {
		return 0xffff
	}
	return c
}

// onesComplement is the internet checksum of one contiguous block.
func onesComplement(b []byte) uint16 {
	return foldChecksum(partialSum(b, 0))
}

// partialSum accumulates b into a running 32-bit sum of 16-bit big-endian
// words, so several blocks can be summed without joining them first. An odd
// final byte is padded on the right, per RFC 1071.
func partialSum(b []byte, sum uint32) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

// foldChecksum folds the carries back in and complements, which is the last
// step of every internet checksum. Looped rather than done twice: folding can
// itself carry, and a single extra fold is the usual shortcut rather than a
// guarantee.
func foldChecksum(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// htons converts to network byte order, which AF_PACKET's sockaddr wants for
// its protocol field. Named rather than inlined because a missing byte swap
// there produces a socket that binds cleanly and carries nothing.
func htons(v uint16) uint16 {
	return v<<8 | v>>8
}
