package mesh

import (
	"encoding/binary"
	"net/netip"
	"time"
)

// Per-family overlay liveness.
//
// A session's own liveness (lastRx, the ctrlPing/ctrlPong keepalive) proves
// the encrypted tunnel to a peer is up — but that keepalive travels inside
// the session as a control message, never touching the overlay IP layer at
// all. It says nothing about whether a packet addressed to that peer's
// overlay v4 or v6 address actually gets delivered once it reaches the
// peer's own TUN and OS: a broken v6 route, a v6 interface that never came
// up, or IPv6 disabled locally on their end all leave the session (and v4)
// completely healthy while v6 silently goes nowhere. That gap is exactly
// what let a redistributed ::/0 default route black-hole this node's own
// outbound v6 traffic while every peer's session looked fine — nothing
// anywhere was checking family-specific deliverability, only "is there a
// session at all" (see onRouteAdd/onHostAdd/onDNSAdd's byNode check, which
// is necessary but not sufficient).
//
// The fix: actually test it, continuously, the same way Info -> Latency's
// on-demand ping already does (ICMP echo), but from inside the engine
// itself so hostsync.go's automatic hosts-file entries can react to it
// without needing anyone to have that page open. sendFamilyProbes injects a
// real ICMP(v6) echo request addressed from this node's own overlay address
// to each peer's, through processOutbound — the exact same pipeline a real
// locally-originated ping takes (egress firewall, NAT-bypass-for-self, the
// works), so the probe is honest about what this node's own current
// configuration actually allows, not just what's cryptographically
// possible. The peer's real OS answers it automatically, standard ICMP
// behavior, no protocol changes needed on their end. deliverInner recognizes
// the reply on the way back in and marks that family good.
const (
	familyProbeInterval = 15 * time.Second        // how often to (re-)probe each configured family
	familyDeadAfter     = 3 * familyProbeInterval // consecutive missed probes before a family reads as down

	// familyProbeICMPID tags gravinet's own probes so they're recognizable in
	// a packet capture; the reply-matching logic below doesn't actually rely
	// on it (see recordFamilyProbeReply's doc comment on why not), so this
	// is for a human reading tcpdump output, not for correctness.
	familyProbeICMPID = 0x9a7a
)

// sendFamilyProbes pings every connected peer's configured overlay
// address(es) — independently per family — from a netState's maintLoop tick,
// same cadence class as sendKeepalive. Best-effort throughout: a peer with
// no address for a family, or this node having none of its own to probe
// from, is simply skipped, not an error.
func (e *Engine) sendFamilyProbes(ns *netState) {
	self4, self6 := ns.selfAddrs()
	if !self4.IsValid() && !self6.IsValid() {
		return
	}
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	now := time.Now().UnixNano()
	for _, ps := range peers {
		if self4.IsValid() && ps.overlay4.IsValid() {
			ps.familyProbeSent4.CompareAndSwap(0, now)
			e.processOutbound(ns, buildICMPv4EchoRequest(self4, ps.overlay4, familyProbeICMPID, 1))
		}
		if self6.IsValid() && ps.overlay6.IsValid() {
			ps.familyProbeSent6.CompareAndSwap(0, now)
			e.processOutbound(ns, buildICMPv6EchoRequest(self6, ps.overlay6, familyProbeICMPID, 1))
		}
	}
}

// recordFamilyProbeReply inspects an inbound, already-decrypted overlay
// packet for an ICMP(v6) echo reply and, if it is one, marks the
// corresponding family good on ps. Called from deliverInner after the
// anti-spoof check has already run, so ip's source is guaranteed to be an
// address ps legitimately owns — that's what makes skipping identifier/
// sequence matching safe here, unlike a real ping tool exposed to arbitrary
// untrusted replies: any echo reply that decrypted successfully under ps's
// session key and passed source-ownership verification can only have come
// from ps itself, however lately it arrives, which is already sufficient
// proof that family is deliverable right now. Best-effort: never blocks or
// mutates the packet, just observes it on the way to deliverInner's own
// dev().Write.
func recordFamilyProbeReply(ps *peerSession, ip []byte) {
	now := time.Now().UnixNano()
	switch {
	case isICMPv4EchoReply(ip) && ps.overlay4.IsValid() && srcIs(ip, ps.overlay4):
		ps.familyGood4.Store(now)
	case isICMPv6EchoReply(ip) && ps.overlay6.IsValid() && srcIs(ip, ps.overlay6):
		ps.familyGood6.Store(now)
	}
}

// srcIs reports whether ip's source address is exactly addr. ip is assumed
// already validated as a well-formed v4/v6 packet by the isICMP*EchoReply
// check that always runs first.
func srcIs(ip []byte, addr netip.Addr) bool {
	if addr.Is4() {
		if len(ip) < 16 {
			return false
		}
		a4 := addr.As4()
		return ip[12] == a4[0] && ip[13] == a4[1] && ip[14] == a4[2] && ip[15] == a4[3]
	}
	if len(ip) < 24 {
		return false
	}
	a16 := addr.As16()
	for i := 0; i < 16; i++ {
		if ip[8+i] != a16[i] {
			return false
		}
	}
	return true
}

// familyLive4/familyLive6 report whether ps's v4/v6 overlay address currently
// looks deliverable: a recent echo reply, or — deliberately optimistic —
// probing that family hasn't been running long enough yet to conclude
// otherwise (a brand-new session, or one still inside its first
// familyDeadAfter window since the first probe). This mirrors hostsync.go's
// existing session-level optimism: a peer shows up the instant its session
// exists, not after some warm-up delay, and family-level gating shouldn't
// introduce a new one either — the goal is catching a family that's
// definitely, persistently down, not delaying a healthy one's appearance
// while its first probe round trip is still in flight. A family with no
// configured address at all (overlay4/overlay6 invalid) is neither live nor
// dead; callers already gate on IsValid() separately (see hostsync.go).
func familyLive4(ps *peerSession, now time.Time) bool {
	return familyLive(ps.familyProbeSent4.Load(), ps.familyGood4.Load(), now)
}

func familyLive6(ps *peerSession, now time.Time) bool {
	return familyLive(ps.familyProbeSent6.Load(), ps.familyGood6.Load(), now)
}

func familyLive(probeSentNanos, goodNanos int64, now time.Time) bool {
	if goodNanos != 0 && now.Sub(time.Unix(0, goodNanos)) < familyDeadAfter {
		return true
	}
	if probeSentNanos == 0 || now.Sub(time.Unix(0, probeSentNanos)) < familyDeadAfter {
		return true // never started probing yet, or still in the initial grace window
	}
	return false
}

// buildICMPv4EchoRequest assembles a type-8 echo request, mirroring
// pmtud_signal.go's buildICMPv4FragNeeded for header conventions and reusing
// its checksum16.
func buildICMPv4EchoRequest(from, to netip.Addr, id, seq uint16) []byte {
	const ipHdr = 20
	const icmpLen = 8
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
	m[0] = 8 // echo request
	m[1] = 0
	binary.BigEndian.PutUint16(m[4:6], id)
	binary.BigEndian.PutUint16(m[6:8], seq)
	binary.BigEndian.PutUint16(m[2:4], checksum16(m))
	return out
}

// buildICMPv6EchoRequest assembles a type-128 echo request, mirroring
// pmtud_signal.go's buildICMPv6TooBig for header conventions and reusing its
// icmpv6Checksum (which covers the pseudo-header, unlike the v4 checksum).
func buildICMPv6EchoRequest(from, to netip.Addr, id, seq uint16) []byte {
	const ipHdr = 40
	const icmpLen = 8
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
	m[0] = 128 // echo request
	m[1] = 0
	binary.BigEndian.PutUint16(m[4:6], id)
	binary.BigEndian.PutUint16(m[6:8], seq)
	binary.BigEndian.PutUint16(m[2:4], icmpv6Checksum(from, to, m))
	return out
}

// isICMPv4EchoReply/isICMPv6EchoReply report whether ip is specifically an
// echo reply (type 0 / type 129) — not a general-purpose ICMP parser, just
// enough validation to safely index into ip for recordFamilyProbeReply.
func isICMPv4EchoReply(ip []byte) bool {
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ip[9] != 1 || len(ip) < ihl+1 { // protocol 1 = ICMP
		return false
	}
	return ip[ihl] == 0 // echo reply
}

func isICMPv6EchoReply(ip []byte) bool {
	if len(ip) < 40 || ip[0]>>4 != 6 {
		return false
	}
	if ip[6] != 58 || len(ip) < 41 { // next header 58 = ICMPv6
		return false
	}
	return ip[40] == 129 // echo reply
}
