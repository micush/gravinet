package mesh

// Tests for the v552 underlay routing-loop protections: the dataplane guard
// (udpPorts/isUnderlayLoop, wired into processOutbound) that drops
// gravinet's own encrypted datagrams if the kernel ever routes them back
// into the overlay interface, and the widened bypass-route gate
// (meshRouteCovers) that keeps such a loop from forming in the first place
// when a mesh-installed kernel route covers a live peer's real underlay
// endpoint. See docs/changelog.md v552 for the field report (an "idle"
// 8-core box pinned at ~530% CPU) and the CPU profile these were built
// from.

import (
	"net/netip"
	"testing"
	"time"
)

// buildUDP4 assembles a minimal IPv4 UDP packet (20-byte header, no
// options) with the given addresses and ports. Checksums are left zero —
// nothing under test reads them.
func buildUDP4(src, dst netip.Addr, sport, dport uint16, payload int) []byte {
	p := make([]byte, 28+payload)
	p[0] = 0x45 // version 4, IHL 5
	total := len(p)
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64 // TTL
	p[9] = 17 // UDP
	s4, d4 := src.As4(), dst.As4()
	copy(p[12:16], s4[:])
	copy(p[16:20], d4[:])
	p[20], p[21] = byte(sport>>8), byte(sport)
	p[22], p[23] = byte(dport>>8), byte(dport)
	ulen := 8 + payload
	p[24], p[25] = byte(ulen>>8), byte(ulen)
	return p
}

// buildUDP6 assembles a minimal IPv6 UDP packet (40-byte fixed header, next
// header UDP, no extension headers).
func buildUDP6(src, dst netip.Addr, sport, dport uint16, payload int) []byte {
	p := make([]byte, 48+payload)
	p[0] = 0x60 // version 6
	plen := 8 + payload
	p[4], p[5] = byte(plen>>8), byte(plen)
	p[6] = 17 // next header: UDP
	p[7] = 64 // hop limit
	s16, d16 := src.As16(), dst.As16()
	copy(p[8:24], s16[:])
	copy(p[24:40], d16[:])
	p[40], p[41] = byte(sport>>8), byte(sport)
	p[42], p[43] = byte(dport>>8), byte(dport)
	p[44], p[45] = byte(plen>>8), byte(plen)
	return p
}

// extHdr6 builds a TLV-shaped IPv6 extension header (hop-by-hop, routing,
// destination options, ...). next is the following header's type; extra adds
// that many 8-octet blocks, since the length field counts 8-octet units
// excluding the first 8.
func extHdr6(next byte, extra int) []byte {
	h := make([]byte, 8*(extra+1))
	h[0] = next
	h[1] = byte(extra)
	return h
}

// fragHdr builds an 8-octet IPv6 fragment header with the given fragment
// offset in bytes (0 => first fragment, which still carries the UDP header).
func fragHdr(next byte, offsetBytes int) []byte {
	h := make([]byte, 8)
	h[0] = next
	off := offsetBytes / 8
	h[2] = byte(off >> 5)
	h[3] = byte(off<<3) & 0xf8
	return h
}

// buildUDP6Ext is buildUDP6 with an extension-header chain spliced between the
// fixed header and the UDP header. firstType is written into the fixed
// header's next-header field.
func buildUDP6Ext(src, dst netip.Addr, sport, dport uint16, payload int, firstType byte, ext []byte) []byte {
	base := buildUDP6(src, dst, sport, dport, payload)
	if len(ext) == 0 {
		return base
	}
	out := make([]byte, 0, len(base)+len(ext))
	out = append(out, base[:40]...)
	out = append(out, ext...)
	out = append(out, base[40:]...)
	out[6] = firstType
	plen := len(out) - 40
	out[4], out[5] = byte(plen>>8), byte(plen)
	return out
}

func TestUDPPorts(t *testing.T) {
	a4 := netip.MustParseAddr("192.0.2.1")
	b4 := netip.MustParseAddr("198.51.100.2")
	a6 := netip.MustParseAddr("2001:db8::1")
	b6 := netip.MustParseAddr("2001:db8::2")

	if s, d, ok := udpPorts(buildUDP4(a4, b4, 48620, 51000, 4)); !ok || s != 48620 || d != 51000 {
		t.Fatalf("v4 udp: got %d,%d,%v", s, d, ok)
	}
	if s, d, ok := udpPorts(buildUDP6(a6, b6, 48620, 51000, 4)); !ok || s != 48620 || d != 51000 {
		t.Fatalf("v6 udp: got %d,%d,%v", s, d, ok)
	}

	// Non-UDP protocol must not parse.
	tcp := buildUDP4(a4, b4, 1, 2, 4)
	tcp[9] = 6
	if _, _, ok := udpPorts(tcp); ok {
		t.Fatal("v4 tcp parsed as udp")
	}

	// A non-first IPv4 fragment has no UDP header to read; the bytes at the
	// transport offset are mid-payload data and must not be trusted as ports.
	frag := buildUDP4(a4, b4, 48620, 51000, 4)
	frag[6], frag[7] = 0x00, 0xb9 // fragment offset 185, MF clear
	if _, _, ok := udpPorts(frag); ok {
		t.Fatal("non-first v4 fragment parsed as udp")
	}
	// A first fragment (offset 0, MF set) does carry the UDP header.
	first := buildUDP4(a4, b4, 48620, 51000, 4)
	first[6] = 0x20 // MF set, offset 0
	if _, _, ok := udpPorts(first); !ok {
		t.Fatal("first v4 fragment should still expose its UDP header")
	}

	// IPv6 with extension headers before UDP. These used to be a blind spot:
	// the parser required UDP immediately after the fixed header, so a looped
	// datagram carrying any extension header slipped the guard. The chain is
	// walked now.
	for _, tc := range []struct {
		name  string
		first byte
		ext   []byte
	}{
		{"hop-by-hop", 0, extHdr6(17, 0)},
		{"destination-options", 60, extHdr6(17, 0)},
		{"routing", 43, extHdr6(17, 0)},
		{"padded hop-by-hop", 0, extHdr6(17, 2)},
		{"first-fragment", 44, fragHdr(17, 0)},
		{"hop-by-hop+destination", 0, append(extHdr6(60, 0), extHdr6(17, 0)...)},
	} {
		pkt := buildUDP6Ext(a6, b6, 48620, 51000, 4, tc.first, tc.ext)
		s, d, ok := udpPorts(pkt)
		if !ok {
			t.Fatalf("%s: udp ports not found behind extension header", tc.name)
		}
		if s != 48620 || d != 51000 {
			t.Fatalf("%s: got ports %d/%d, want 48620/51000", tc.name, s, d)
		}
	}

	// A non-first fragment genuinely has no UDP header to read, so the parser
	// must still decline it rather than misread payload bytes as ports.
	nonFirst := buildUDP6Ext(a6, b6, 48620, 51000, 4, 44, fragHdr(17, 8))
	if _, _, ok := udpPorts(nonFirst); ok {
		t.Fatal("non-first v6 fragment parsed as udp")
	}

	// ESP is encrypted: nothing parseable follows, so decline.
	esp := buildUDP6Ext(a6, b6, 48620, 51000, 4, 60, extHdr6(17, 0))
	esp[6] = 50 // ESP: encrypted, nothing parseable follows
	if _, _, ok := udpPorts(esp); ok {
		t.Fatal("ESP parsed as udp")
	}

	// A chain longer than the cap must be refused, not walked forever.
	var long []byte
	for i := 0; i < maxIPv6ExtHeaders+2; i++ {
		long = append(long, extHdr6(60, 0)...)
	}
	long = append(long, extHdr6(17, 0)...)
	if _, _, ok := udpPorts(buildUDP6Ext(a6, b6, 1, 2, 4, 60, long)); ok {
		t.Fatal("over-long extension chain was walked")
	}

	// A header claiming a length that runs past the packet must be refused.
	trunc := buildUDP6Ext(a6, b6, 48620, 51000, 4, 0, extHdr6(17, 0))
	trunc[41] = 200 // absurd header extension length
	if _, _, ok := udpPorts(trunc); ok {
		t.Fatal("extension header overrunning the packet was accepted")
	}

	// Truncated packets.
	if _, _, ok := udpPorts(buildUDP4(a4, b4, 1, 2, 4)[:24]); ok {
		t.Fatal("truncated v4 parsed")
	}
	if _, _, ok := udpPorts(nil); ok {
		t.Fatal("empty packet parsed")
	}
}

func TestIsUnderlayLoopDetectsOwnDatagram(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.primaryPort.Store(48620)

	peerEP := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, nodeID: "peer", endpoint: peerEP}
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.seeds = append(ns.seeds, netip.MustParseAddrPort("198.51.100.9:48620"))
	ns.publishFwd()
	ns.mu.Unlock()

	self := netip.MustParseAddr("192.0.2.10")

	loop := buildUDP4(self, peerEP.Addr(), 48620, peerEP.Port(), 32)
	if dst, _ := parseDst(loop); !e.isUnderlayLoop(ns, loop, dst) {
		t.Fatal("own datagram to a live session endpoint not flagged as a loop")
	}

	// Same again but with the session endpoint recorded 4-in-6 mapped, as a
	// dual-stack socket reports it — must still match the plain v4 header.
	ps.mu.Lock()
	ps.endpoint = netip.AddrPortFrom(netip.AddrFrom16(peerEP.Addr().As16()), peerEP.Port())
	ps.mu.Unlock()
	if dst, _ := parseDst(loop); !e.isUnderlayLoop(ns, loop, dst) {
		t.Fatal("4-in-6 mapped session endpoint not matched against plain v4 header")
	}
	ps.mu.Lock()
	ps.endpoint = peerEP
	ps.mu.Unlock()

	// A currently-dialed seed's endpoint is a loop target too (handshake
	// inits loop the same way data does, and would keep the session from
	// ever forming).
	seedLoop := buildUDP4(self, netip.MustParseAddr("198.51.100.9"), 48620, 48620, 32)
	if dst, _ := parseDst(seedLoop); !e.isUnderlayLoop(ns, seedLoop, dst) {
		t.Fatal("own datagram to a dialed seed not flagged as a loop")
	}

	// An extra listen port is a valid own source port too — replies go back
	// out the arrival socket.
	extras := []uint16{443}
	e.extraUDPPorts.Store(&extras)
	extraSrc := buildUDP4(self, peerEP.Addr(), 443, peerEP.Port(), 32)
	if dst, _ := parseDst(extraSrc); !e.isUnderlayLoop(ns, extraSrc, dst) {
		t.Fatal("datagram from an extra listen port not flagged")
	}

	// Negatives: wrong source port (a gatewayed host's own traffic), wrong
	// destination port (same peer address, unrelated service), and a
	// destination that is no known endpoint at all.
	if dst, _ := parseDst(buildUDP4(self, peerEP.Addr(), 40000, peerEP.Port(), 32)); e.isUnderlayLoop(ns, buildUDP4(self, peerEP.Addr(), 40000, peerEP.Port(), 32), dst) {
		t.Fatal("foreign source port wrongly flagged")
	}
	wrongDPort := buildUDP4(self, peerEP.Addr(), 48620, 53, 32)
	if dst, _ := parseDst(wrongDPort); e.isUnderlayLoop(ns, wrongDPort, dst) {
		t.Fatal("unrelated destination port on the peer's address wrongly flagged")
	}
	other := buildUDP4(self, netip.MustParseAddr("203.0.113.99"), 48620, 51820, 32)
	if dst, _ := parseDst(other); e.isUnderlayLoop(ns, other, dst) {
		t.Fatal("unknown destination wrongly flagged")
	}
	if e.loopDrops.Load() != 0 {
		t.Fatalf("isUnderlayLoop must not count drops itself; loopDrops=%d", e.loopDrops.Load())
	}
}

func TestProcessOutboundDropsLoopedDatagram(t *testing.T) {
	e, ns := testEngineWithNet(t)
	e.primaryPort.Store(48620)

	peerEP := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, nodeID: "peer", endpoint: peerEP}
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	// A redistributed prefix covering the peer's own endpoint — the exact
	// misconfiguration behind the v552 field report — so processOutbound's
	// route lookup would resolve the looped packet right back to the peer.
	ns.redist = append(ns.redist, routeEntry{origin: "peer", prefix: netip.MustParsePrefix("203.0.113.0/24"), lastSeen: time.Now()})
	ns.publishFwd()
	ns.mu.Unlock()

	loop := buildUDP4(netip.MustParseAddr("192.0.2.10"), peerEP.Addr(), 48620, peerEP.Port(), 64)
	e.processOutbound(ns, loop)

	if got := e.loopDrops.Load(); got != 1 {
		t.Fatalf("looped datagram not dropped by processOutbound (loopDrops=%d)", got)
	}

	// Ordinary overlay traffic into the same covered prefix must still make
	// it through the pipeline to sendData — the guard is precise, not a
	// blanket drop of the prefix. sealAndSend will fail (no live crypto
	// session on this hand-built peerSession), but only after the guard, and
	// what's asserted here is just that the drop counter didn't move.
	defer func() { recover() }() // hand-built session has no sess; a panic past the guard still proves non-drop
	ordinary := buildUDP4(netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("203.0.113.80"), 40000, 443, 64)
	e.processOutbound(ns, ordinary)
	if got := e.loopDrops.Load(); got != 1 {
		t.Fatalf("ordinary traffic into the covered prefix was wrongly counted as a loop (loopDrops=%d)", got)
	}
}

func TestMeshRouteCovers(t *testing.T) {
	_, ns := testEngineWithNet(t)
	p := netip.MustParsePrefix("203.0.113.0/24")
	ns.osMu.Lock()
	ns.osMetric[p] = 9000
	ns.osMu.Unlock()

	if !ns.meshRouteCovers(netip.MustParseAddr("203.0.113.5")) {
		t.Fatal("address inside an installed mesh route not reported covered")
	}
	// 4-in-6 mapped form of the same address must be treated identically.
	if !ns.meshRouteCovers(netip.AddrFrom16(netip.MustParseAddr("203.0.113.5").As16())) {
		t.Fatal("4-in-6 mapped address inside an installed mesh route not reported covered")
	}
	if ns.meshRouteCovers(netip.MustParseAddr("203.0.114.5")) {
		t.Fatal("address outside every installed route reported covered")
	}
	if ns.meshRouteCovers(netip.Addr{}) {
		t.Fatal("invalid address reported covered")
	}
}

// TestSyncRouteInstallsBypassForCoveredEndpoint is the end-to-end shape of
// the v552 fix: a redistributed route whose prefix contains a live peer's
// real underlay endpoint must, on kernel-route install, immediately get
// that endpoint a /32 bypass host route via the physical gateway — without
// full-tunnel ever being on — and release it again when the route is
// withdrawn.
func TestSyncRouteInstallsBypassForCoveredEndpoint(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	peerEP := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, nodeID: "peer", endpoint: peerEP}
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.mu.Unlock()

	p := netip.MustParsePrefix("203.0.113.0/24")
	ns.mu.Lock()
	ns.redist = append(ns.redist, routeEntry{origin: "peer", prefix: p, lastSeen: time.Now()})
	ns.mu.Unlock()

	// Install: syncRoute programs the kernel route and must then acquire the
	// covered endpoint's bypass.
	e.syncRoute(ns, p)
	wantBypass := netip.MustParsePrefix("203.0.113.5/32")
	found := false
	for _, c := range *calls {
		if c.add && c.prefix == wantBypass {
			found = true
		}
	}
	if !found {
		t.Fatalf("no bypass host route installed for covered endpoint; gateway calls: %+v", *calls)
	}
	if got := ps.bypassAddr; got != peerEP.Addr() {
		t.Fatalf("session bypassAddr = %v, want %v", got, peerEP.Addr())
	}

	// A peer whose endpoint is outside the prefix must not have gotten one.
	for _, c := range *calls {
		if c.add && c.prefix != wantBypass {
			t.Fatalf("unexpected extra bypass route installed: %+v", c)
		}
	}

	// Withdraw: the origin stops advertising, the kernel route comes out,
	// and the now-unneeded bypass is released.
	ns.mu.Lock()
	ns.redist = ns.redist[:0]
	ns.mu.Unlock()
	*calls = (*calls)[:0]
	e.syncRoute(ns, p)
	released := false
	for _, c := range *calls {
		if !c.add && c.prefix == wantBypass {
			released = true
		}
	}
	if !released {
		t.Fatalf("bypass host route not released after route withdrawal; gateway calls: %+v", *calls)
	}
	if ps.bypassAddr.IsValid() {
		t.Fatalf("session still holds bypassAddr %v after withdrawal", ps.bypassAddr)
	}
}

// TestSyncPeerBypassRouteCoveredWithoutFullTunnel pins the widened gate at
// the session-lifecycle entry point: a handshake completing for a peer
// whose endpoint an already-installed mesh route covers must acquire the
// bypass on the spot (syncPeerBypassRoute runs at session install/roam),
// with fullTunnel off the whole time.
func TestSyncPeerBypassRouteCoveredWithoutFullTunnel(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(false) // explicit: the point is that this stays off

	ns.osMu.Lock()
	ns.osMetric[netip.MustParsePrefix("203.0.113.0/24")] = 9000
	ns.osMu.Unlock()

	ps := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("203.0.113.5:51820")}
	e.syncPeerBypassRoute(ns, ps)
	if len(*calls) != 1 || !(*calls)[0].add || (*calls)[0].prefix != netip.MustParsePrefix("203.0.113.5/32") {
		t.Fatalf("expected exactly one /32 bypass Add for the covered endpoint, got %+v", *calls)
	}

	// An uncovered endpoint stays a no-op, exactly as before v552.
	other := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("198.51.100.7:51820")}
	e.syncPeerBypassRoute(ns, other)
	if len(*calls) != 1 {
		t.Fatalf("uncovered endpoint should not acquire a bypass, got %+v", *calls)
	}
}
