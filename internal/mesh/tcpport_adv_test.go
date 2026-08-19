package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestHSPayloadCarriesTCPPort: the TCP port survives a handshake round-trip
// alongside the web port, and an older payload without it decodes to 0.
func TestHSPayloadCarriesTCPPort(t *testing.T) {
	in := hsPayload{
		Index:     7,
		Ephemeral: make([]byte, ephemeralLen),
		NodeID:    "n",
		Hostname:  "h",
		Managed:   true,
		WebPort:   8443,
		TCPPort:   443,
	}
	enc := encodeHSPayload(in)
	out, err := decodeHSPayload(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TCPPort != 443 || out.WebPort != 8443 || !out.Managed {
		t.Fatalf("round-trip wrong: tcp=%d web=%d managed=%v", out.TCPPort, out.WebPort, out.Managed)
	}
	// Simulate an older peer that doesn't send the TCP port: truncate right
	// after WebPort, dropping everything the nested-optional-field chain adds
	// after it (see decodeHSPayload) — TCPPort itself, the two extra-port
	// lists, LocalEndpoints, BGPASN, and Version — computed from each field's own
	// exact encoded size rather than a magic byte count, which is what let
	// this test silently stop testing what it claimed to (see
	// bgpASNFieldLen's sibling comment below) the last time a field was
	// added after TCPPort without this being updated to match.
	const tcpPortFieldLen = 2
	const bgpASNFieldLen = 4
	trailingLen := tcpPortFieldLen +
		portListEncodedLen(in.ExtraTCPPorts) +
		portListEncodedLen(in.ExtraUDPPorts) +
		endpointListEncodedLen(in.LocalEndpoints) +
		bgpASNFieldLen +
		lenStrEncodedLen(in.Version)
	old, err := decodeHSPayload(enc[:len(enc)-trailingLen])
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if old.TCPPort != 0 || old.WebPort != 8443 {
		t.Fatalf("backward-compat wrong: tcp=%d web=%d", old.TCPPort, old.WebPort)
	}
}

// TestPeerListCarriesTCPPort: the gossip list carries per-entry TCP ports,
// and a list without the trailing block decodes them as 0.
func TestPeerListCarriesTCPPort(t *testing.T) {
	in := []peerEntry{
		{nodeID: "A", hostname: "a", overlay4: netip.MustParseAddr("10.0.0.1"),
			endpoint: netip.MustParseAddrPort("198.51.100.7:65432"), tcpPort: 443},
		{nodeID: "B", hostname: "b", overlay4: netip.MustParseAddr("10.0.0.2"),
			endpoint: netip.MustParseAddrPort("198.51.100.8:65432"), tcpPort: 8443},
	}
	enc := encodePeerList(in)
	out, err := decodePeerList(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].tcpPort != 443 || out[1].tcpPort != 8443 {
		t.Fatalf("tcp ports not carried: %+v", out)
	}
	// Strip the trailing TCP block (1 marker + 2 bytes per entry): an older
	// decoder/peer simply sees no ports.
	old, err := decodePeerList(enc[:len(enc)-(1+2*len(in))])
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if len(old) != 2 || old[0].tcpPort != 0 || old[1].tcpPort != 0 {
		t.Fatalf("backward-compat wrong: %+v", old)
	}
}

// TestEnsureTCPDiscoversPortFromLiveSessionOnDifferentPort reproduces
// the production scenario directly: a peer is already connected (e.g. its
// live session settled on a TCP port learned dynamically), but a
// separate, stale seed entry for the same IP — e.g. an operator-configured
// bootstrap address using the default port — keeps getting retried on the
// wrong port and can never complete a handshake there, producing an
// indefinite "no mesh session formed" warning even though the peer is
// perfectly reachable. tcpPortForEndpoint's IP-based match on the live
// session should let this stale seed discover and dial the peer's actual
// working port instead of blindly guessing the default.
func TestEnsureTCPDiscoversPortFromLiveSessionOnDifferentPort(t *testing.T) {
	e, f, ns := tcpEngine(t, 65432) // our own default port is 65432
	ip := netip.MustParseAddr("203.0.113.7")
	liveEndpoint := netip.AddrPortFrom(ip, 443) // where the peer actually is
	staleSeed := netip.AddrPortFrom(ip, 65432)  // e.g. a stale configured seed

	ns.mu.Lock()
	ns.byNode["peerX"] = &peerSession{net: ns, nodeID: "peerX", endpoint: liveEndpoint, tcpPort: 443}
	ns.mu.Unlock()

	e.ensureTCP(ns, staleSeed)
	// The port discovery, seedTCP recording, and connectedTo
	// short-circuit all happen synchronously within ensureTCP itself —
	// only the actual dial (which correctly never happens here) would be
	// asynchronous, so no wait is needed before asserting.
	if d := f.dials(); len(d) != 0 {
		t.Fatalf("expected no dial (already connected at the discovered port), got %v", d)
	}
	if !e.connectedTo(ns, staleSeed) {
		t.Fatal("stale seed on the wrong port should now be recognized as satisfied via the live session's actual endpoint")
	}
}

// TestEnsureTCPUsesAdvertisedPort: when a peer's advertised TCP port is
// known (here via node info) and differs from our own, the engine dials *that*
// port — proving nodes don't need a shared port.
func TestEnsureTCPUsesAdvertisedPort(t *testing.T) {
	e, f, ns := tcpEngine(t, 65432) // our own port is 65432
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	// The peer at this endpoint advertises a *different* TCP port.
	ns.mu.Lock()
	ns.nodes["peerX"] = &nodeInfo{nodeID: "peerX", endpoint: seed, tcpPort: 8443, lastSeen: time.Now()}
	ns.mu.Unlock()

	e.ensureTCP(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// Containment, not exclusivity. The flat candidate model dials every
	// candidate rather than picking one: the seed's own port and the peer's
	// advertised port are both plausible and both cheap to try, and insisting
	// on exactly one dial is what forced the old code to *choose* — which is
	// how it ended up borrowing a port from an unrelated session at the same
	// IP. What matters is that the advertised port is among them.
	want := netip.MustParseAddrPort("203.0.113.7:8443")
	if !dialedContains(f, want) {
		t.Fatalf("expected a dial to advertised port %s, got %v", want, f.dials())
	}
	// fb differs from the seed, so it should be added as a seed.
	ns.mu.RLock()
	seeds := append([]netip.AddrPort(nil), ns.seeds...)
	ns.mu.RUnlock()
	found := false
	for _, s := range seeds {
		if s == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("advertised-port endpoint not added as seed: %v", seeds)
	}
}

// TestEnsureTCPUsesSeedHint: with no advertised port known yet (cold start),
// the engine dials the join-token seed hint rather than its own port.
func TestEnsureTCPUsesSeedHint(t *testing.T) {
	e := NewEngine(Options{
		NodeID:  "self",
		TCPPort: 65432, // our own port
		Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24"), SeedTCPPort: 443}}, // token hint
	})
	f := &fakeTCP{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.netSnapshot()[1]

	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	e.ensureTCP(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// The hint must be dialed; the seed's own port being dialed alongside it
	// is the intended flat-model behaviour, not a failure.
	want := netip.MustParseAddrPort("203.0.113.7:443")
	if !dialedContains(f, want) {
		t.Fatalf("expected a dial to seed-hint port %s, got %v", want, f.dials())
	}
}

// dialedContains waits briefly for want to appear among the dials. The dials
// run in goroutines, and with several candidates in flight the one under test
// is not necessarily first.
func dialedContains(f *fakeTCP, want netip.AddrPort) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range f.dials() {
			if d == want {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
