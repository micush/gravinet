package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// ipv4From builds a minimal well-formed IPv4 packet with the given source and
// destination, enough for parseSrc/parseDst and deliverInner's checks.
func ipv4From(src, dst netip.Addr) []byte {
	p := make([]byte, 20)
	p[0] = 0x45
	total := uint16(20)
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64 // TTL
	p[9] = 17 // UDP
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	return p
}

func newSpoofTestEngine(t *testing.T) (*Engine, *netState, *fakeDev) {
	t.Helper()
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: dev, Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	return e, ns, dev
}

// addPeerWithOverlay registers a peer session owning a single overlay v4 addr,
// wired into the route map the way install() would.
func addPeerWithOverlay(ns *netState, id string, overlay netip.Addr) *peerSession {
	ps := &peerSession{net: ns, nodeID: id, endpoint: netip.MustParseAddrPort("203.0.113.1:65432"), overlay4: overlay}
	ns.mu.Lock()
	ns.byNode[id] = ps
	ns.routes4[overlay] = ps
	ns.mu.Unlock()
	return ps
}

// A peer sending a packet sourced from its OWN overlay address is delivered.
func TestDeliverInnerAllowsOwnSource(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps := addPeerWithOverlay(ns, "peerA", own)

	pkt := ipv4From(own, netip.MustParseAddr("10.0.0.9"))
	engine.deliverInner(ps, pkt, len(pkt))

	select {
	case <-dev.out:
		// delivered, as expected
	case <-time.After(time.Second):
		t.Fatal("packet from peer's own overlay source was not delivered")
	}
	if ps.spoofDrop.Load() != 0 {
		t.Fatalf("spoofDrop=%d, want 0 for a legitimate source", ps.spoofDrop.Load())
	}
}

// A peer sending a packet sourced from ANOTHER node's overlay address is
// dropped (the core anti-spoofing guard) and counted.
func TestDeliverInnerDropsSpoofedSource(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	other := netip.MustParseAddr("10.0.0.6")
	ps := addPeerWithOverlay(ns, "peerA", own)
	// A different peer legitimately owns `other`.
	addPeerWithOverlay(ns, "peerB", other)

	// peerA tries to source from peerB's address.
	pkt := ipv4From(other, netip.MustParseAddr("10.0.0.9"))
	engine.deliverInner(ps, pkt, len(pkt))

	select {
	case <-dev.out:
		t.Fatal("spoofed packet (peerA sourcing peerB's overlay addr) was delivered; anti-spoofing failed")
	case <-time.After(150 * time.Millisecond):
		// dropped, as expected
	}
	if ps.spoofDrop.Load() != 1 {
		t.Fatalf("spoofDrop=%d, want 1", ps.spoofDrop.Load())
	}
}

// A peer that is the gateway for a redistributed-route prefix may source
// traffic from inside that prefix, even though it isn't the peer's own address.
func TestDeliverInnerAllowsRedistributedPrefixSource(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps := addPeerWithOverlay(ns, "gwPeer", own)
	// gwPeer advertises 192.168.50.0/24 as a redistributed route (it's the GW).
	gwPrefix := netip.MustParsePrefix("192.168.50.0/24")
	ns.mu.Lock()
	ns.redist = append(ns.redist, routeEntry{origin: "gwPeer", prefix: gwPrefix, metric: 1, lastSeen: time.Now()})
	ns.mu.Unlock()

	// A host behind the gateway, inside the advertised prefix.
	pkt := ipv4From(netip.MustParseAddr("192.168.50.10"), netip.MustParseAddr("10.0.0.9"))
	engine.deliverInner(ps, pkt, len(pkt))

	select {
	case <-dev.out:
		// delivered, as expected
	case <-time.After(time.Second):
		t.Fatal("packet from a prefix the peer is the gateway for was dropped")
	}
	if ps.spoofDrop.Load() != 0 {
		t.Fatalf("spoofDrop=%d, want 0 for a legitimate gateway source", ps.spoofDrop.Load())
	}
}

// A peer may not source another peer's exact overlay *identity* even if that
// address were somehow also inside a prefix — identity impersonation is the one
// thing the guard always blocks. (Sourcing from inside another peer's gatewayed
// prefix, by contrast, is intentionally allowed: it is not identity spoofing,
// and blocking it would break NAT/masquerade, see sourceAllowedFrom.)
func TestDeliverInnerDropsOtherPeersOverlayIdentity(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	ps := addPeerWithOverlay(ns, "peerA", netip.MustParseAddr("10.0.0.5"))
	// peerB owns 10.0.0.6 as its overlay identity.
	addPeerWithOverlay(ns, "peerB", netip.MustParseAddr("10.0.0.6"))

	// peerA tries to source from peerB's overlay identity.
	pkt := ipv4From(netip.MustParseAddr("10.0.0.6"), netip.MustParseAddr("10.0.0.9"))
	engine.deliverInner(ps, pkt, len(pkt))

	select {
	case <-dev.out:
		t.Fatal("peerA impersonated peerB's overlay identity; must be dropped")
	case <-time.After(150 * time.Millisecond):
	}
	if ps.spoofDrop.Load() != 1 {
		t.Fatalf("spoofDrop=%d, want 1", ps.spoofDrop.Load())
	}
}

// A NAT/masquerade or gateway translate source that no other peer claims is
// allowed — this is the case strict per-address allow-listing wrongly broke.
func TestDeliverInnerAllowsUnclaimedTranslateSource(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	ps := addPeerWithOverlay(ns, "gwPeer", netip.MustParseAddr("10.0.0.5"))

	// A masqueraded/gatewayed source outside the overlay that no peer owns.
	pkt := ipv4From(netip.MustParseAddr("172.16.0.9"), netip.MustParseAddr("10.0.0.9"))
	engine.deliverInner(ps, pkt, len(pkt))

	select {
	case <-dev.out:
		// delivered, as expected
	case <-time.After(time.Second):
		t.Fatal("a translate/masquerade source no other peer claims was dropped")
	}
	if ps.spoofDrop.Load() != 0 {
		t.Fatalf("spoofDrop=%d, want 0 for an unclaimed translate source", ps.spoofDrop.Load())
	}
}

// An unparseable inner packet is dropped (fail closed), not delivered.
func TestDeliverInnerDropsUnparseable(t *testing.T) {
	engine, ns, dev := newSpoofTestEngine(t)
	ps := addPeerWithOverlay(ns, "peerA", netip.MustParseAddr("10.0.0.5"))
	engine.deliverInner(ps, []byte{0x00}, 1) // too short / bad version nibble

	select {
	case <-dev.out:
		t.Fatal("unparseable packet was delivered; should fail closed")
	case <-time.After(150 * time.Millisecond):
	}
	if ps.spoofDrop.Load() != 1 {
		t.Fatalf("spoofDrop=%d, want 1", ps.spoofDrop.Load())
	}
}

// ---- handshake replay ----

func TestHSReplayRejectsDuplicateEphemeral(t *testing.T) {
	_, ns, _ := newSpoofTestEngine(t)
	eph, err := crypto.NewEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	pub := eph.Public()
	now := time.Now()

	if ns.hsReplay(pub, now) {
		t.Fatal("first sight of an ephemeral must be accepted (not a replay)")
	}
	if !ns.hsReplay(pub, now) {
		t.Fatal("second sight of the same ephemeral within the window must be rejected as a replay")
	}
}

func TestHSReplayDistinctEphemeralsBothAccepted(t *testing.T) {
	_, ns, _ := newSpoofTestEngine(t)
	e1, _ := crypto.NewEphemeral()
	e2, _ := crypto.NewEphemeral()
	now := time.Now()
	if ns.hsReplay(e1.Public(), now) {
		t.Fatal("first ephemeral wrongly flagged as replay")
	}
	if ns.hsReplay(e2.Public(), now) {
		t.Fatal("a distinct ephemeral must be accepted, not flagged as a replay of the first")
	}
}

func TestHSReplayEntryLapsesAfterWindow(t *testing.T) {
	_, ns, _ := newSpoofTestEngine(t)
	eph, _ := crypto.NewEphemeral()
	pub := eph.Public()
	now := time.Now()
	if ns.hsReplay(pub, now) {
		t.Fatal("first sight must be accepted")
	}
	// Past the skew window, the same ephemeral is no longer cached, so it (and
	// freshTimestamp) would both treat it as fresh again — consistent horizons.
	later := now.Add(clockSkew + time.Second)
	if ns.hsReplay(pub, later) {
		t.Fatal("an ephemeral seen longer ago than the skew window must not still count as a replay")
	}
}

// The replay cache must stay bounded under a flood of unique ephemerals.
func TestHSReplayCacheBounded(t *testing.T) {
	_, ns, _ := newSpoofTestEngine(t)
	now := time.Now()
	for i := 0; i < maxHSSeen*2; i++ {
		e, err := crypto.NewEphemeral()
		if err != nil {
			t.Fatal(err)
		}
		ns.hsReplay(e.Public(), now)
	}
	ns.hsSeenMu.Lock()
	n := len(ns.hsSeen)
	ns.hsSeenMu.Unlock()
	if n > maxHSSeen {
		t.Fatalf("hsSeen grew to %d, want <= %d (bounded)", n, maxHSSeen)
	}
}
