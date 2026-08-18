package mesh

import (
	"encoding/binary"
	"net/netip"
	"time"
)

// Path-MTU signalling toward the local sender.
//
// Overlay fragmentation (frag.go) makes an oversized packet *work*, but it
// does not make it stop happening. A host on a jumbo-MTU segment sending to a
// peer whose underlay path is ~1500 keeps emitting 8900-byte packets forever,
// and every one is split into seven datagrams that all have to arrive. Losing
// any single one discards the whole packet, so ordinary path loss is amplified
// by the fragment count — and on a leg carrying relayed sessions, the
// keepalives riding it starve and the sessions get reaped.
//
// The fix is the one the IP stack already knows how to act on: tell the sender.
// A TCP sender that receives an ICMP "fragmentation needed" (or ICMPv6 "packet
// too big") lowers its MSS once and stops generating oversized packets
// entirely, which removes the amplification at the source instead of absorbing
// it downstream. Nothing here changes what gravinet forwards: the packet is
// still fragmented and still delivered, exactly as before. This only adds the
// advisory, so a sender that ignores it is no worse off than it was.
//
// Deliberately conservative in three ways:
//
//   - IPv4 only when DF is set. Without DF the sender has explicitly permitted
//     fragmentation, and RFC 1191 signalling would be both unsolicited and
//     misleading. Stacks doing PMTUD set DF, which is the case that matters.
//   - Never in reply to an ICMP error, which is how ICMP storms start.
//   - Rate-limited per session, since the trigger is per-packet and a bulk
//     transfer would otherwise generate one advisory per datagram.
const (
	tooBigMinInterval = time.Second // per-session floor between advisories
	icmpV6MinMTU      = 1280        // RFC 8200: never advertise below this
	icmpV4MinMTU      = 68          // RFC 791 minimum reassembly buffer
)

// signalPacketTooBig advises the sender of pkt that mtu is the largest packet
// that will cross to this peer without fragmentation. Best-effort throughout:
// every failure path simply declines to advise, because the packet itself is
// forwarded regardless and a missing advisory costs only efficiency.
func (e *Engine) signalPacketTooBig(ps *peerSession, pkt []byte, mtu int) {
	ns := ps.net
	if ns == nil || len(pkt) < 20 || mtu <= 0 {
		return
	}
	src, ok := parseSrc(pkt)
	if !ok || !src.IsValid() {
		return
	}
	self4, self6 := ns.selfAddrs()

	// Never advise this node's own stack. The advisory names self4/self6 as
	// its source, so for a packet this host originated the message is
	// self-addressed — src == dst == our own overlay address, arriving on
	// our own overlay device. The two families then diverge, and neither
	// outcome is the one intended:
	//
	//   - IPv4 is discarded before it reaches the PMTU cache, as a source
	//     address that is one of our own local addresses arriving from
	//     "outside" (fib_validate_source; see rp_filter/accept_local). The
	//     advisory has no effect at all, and only appears to be harmless.
	//   - IPv6 has no equivalent source check. It is believed, and installs
	//     a per-destination route exception, after which every DONTFRAG
	//     send above that size fails locally with EMSGSIZE — even though
	//     sendFragmented would have split the packet and delivered it. The
	//     overlay's whole jumbo-MTU premise is defeated for the one host
	//     that can least afford it.
	//
	// The advisory exists for senders *behind* this node — the redistributed
	// jumbo LAN in the v706 field case — which are unaffected by this and
	// still receive it. Checked before the rate limiter so local traffic
	// cannot consume the per-session token that a forwarded sender needs.
	if src == self4 || src == self6 {
		return
	}

	now := time.Now().UnixNano()
	last := ps.lastTooBig.Load()
	if now-last < int64(tooBigMinInterval) {
		return
	}
	if !ps.lastTooBig.CompareAndSwap(last, now) {
		return // another goroutine just sent one
	}

	var icmp []byte
	switch {
	case pkt[0]>>4 == 4:
		if !ipv4DontFragment(pkt) || ipv4IsICMPError(pkt) {
			return
		}
		if mtu < icmpV4MinMTU || !self4.IsValid() || !src.Is4() {
			return
		}
		icmp = buildICMPv4FragNeeded(self4, src, pkt, mtu)
	case pkt[0]>>4 == 6:
		if len(pkt) < 40 || ipv6IsICMPError(pkt) {
			return
		}
		if !self6.IsValid() || !src.Is6() {
			return
		}
		// A PTB below the v6 minimum is not actionable by the sender; it
		// would fragment at 1280 itself, which is what already happens.
		if mtu < icmpV6MinMTU {
			mtu = icmpV6MinMTU
		}
		icmp = buildICMPv6TooBig(self6, src, pkt, mtu)
	default:
		return
	}
	if icmp == nil {
		return
	}
	if _, err := ns.dev().Write(icmp); err != nil {
		e.log.Debugf("mesh: path-mtu advisory to %s: %v", src, err)
		return
	}
	ps.tooBigSent.Add(1)
}

func ipv4DontFragment(pkt []byte) bool { return pkt[6]&0x40 != 0 }

// ipv4IsICMPError reports whether pkt is itself an ICMP error message. Replying
// to one with another is how ICMP storms start.
func ipv4IsICMPError(pkt []byte) bool {
	ihl := int(pkt[0]&0x0f) * 4
	if pkt[9] != 1 || len(pkt) < ihl+1 {
		return false
	}
	switch pkt[ihl] {
	case 3, 4, 5, 11, 12: // unreachable, quench, redirect, time exceeded, param problem
		return true
	}
	return false
}

func ipv6IsICMPError(pkt []byte) bool {
	// Only the unextended case: a packet carrying extension headers is not
	// worth walking here, and declining to advise is always safe.
	if pkt[6] != 58 || len(pkt) < 41 {
		return false
	}
	return pkt[40] < 128 // ICMPv6 error types are 0-127; 128+ are informational
}

// buildICMPv4FragNeeded assembles a type 3 code 4 message quoting the original
// header plus 8 bytes, per RFC 792/1191.
func buildICMPv4FragNeeded(from, to netip.Addr, orig []byte, mtu int) []byte {
	quote := len(orig)
	if quote > 28 {
		quote = 28
	}
	const ipHdr = 20
	icmpLen := 8 + quote
	out := make([]byte, ipHdr+icmpLen)

	out[0] = 0x45
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	out[8] = 64 // TTL
	out[9] = 1  // ICMP
	a4 := from.As4()
	copy(out[12:16], a4[:])
	b4 := to.As4()
	copy(out[16:20], b4[:])
	binary.BigEndian.PutUint16(out[10:12], checksum16(out[:ipHdr]))

	m := out[ipHdr:]
	m[0] = 3 // destination unreachable
	m[1] = 4 // fragmentation needed and DF set
	binary.BigEndian.PutUint16(m[6:8], uint16(mtu))
	copy(m[8:], orig[:quote])
	binary.BigEndian.PutUint16(m[2:4], checksum16(m))
	return out
}

// buildICMPv6TooBig assembles a type 2 message, quoting as much of the original
// as keeps the whole thing within the v6 minimum MTU, per RFC 4443.
func buildICMPv6TooBig(from, to netip.Addr, orig []byte, mtu int) []byte {
	const ipHdr = 40
	quote := len(orig)
	if max := icmpV6MinMTU - ipHdr - 8; quote > max {
		quote = max
	}
	icmpLen := 8 + quote
	out := make([]byte, ipHdr+icmpLen)

	out[0] = 0x60
	binary.BigEndian.PutUint16(out[4:6], uint16(icmpLen))
	out[6] = 58 // ICMPv6
	out[7] = 64 // hop limit
	a16 := from.As16()
	copy(out[8:24], a16[:])
	b16 := to.As16()
	copy(out[24:40], b16[:])

	m := out[ipHdr:]
	m[0] = 2 // packet too big
	binary.BigEndian.PutUint32(m[4:8], uint32(mtu))
	copy(m[8:], orig[:quote])
	binary.BigEndian.PutUint16(m[2:4], icmpv6Checksum(from, to, m))
	return out
}

// checksum16 is the standard internet checksum over b.
func checksum16(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// icmpv6Checksum covers the IPv6 pseudo-header as well as the message, which
// is what makes it different from its v4 counterpart.
func icmpv6Checksum(src, dst netip.Addr, msg []byte) uint16 {
	pseudo := make([]byte, 40)
	s16 := src.As16()
	copy(pseudo[0:16], s16[:])
	d16 := dst.As16()
	copy(pseudo[16:32], d16[:])
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(len(msg)))
	pseudo[39] = 58
	return checksum16(append(pseudo, msg...))
}
