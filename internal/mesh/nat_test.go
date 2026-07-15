package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

func makeUDP(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	ihl := 20
	p := make([]byte, ihl+8+len(payload))
	p[0] = 0x45
	total := uint16(len(p))
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64
	p[9] = 17
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[ihl], p[ihl+1] = byte(sport>>8), byte(sport)
	p[ihl+2], p[ihl+3] = byte(dport>>8), byte(dport)
	ulen := uint16(8 + len(payload))
	p[ihl+4], p[ihl+5] = byte(ulen>>8), byte(ulen)
	copy(p[ihl+8:], payload)
	fixChecksums(p, ihl)
	return p
}

func ipValid(pkt []byte) bool {
	ihl := int(pkt[0]&0x0f) * 4
	return ones(pkt[:ihl], 0) == 0
}

func udpValid(pkt []byte) bool {
	ihl := int(pkt[0]&0x0f) * 4
	l4 := pkt[ihl:]
	var pseudo uint32
	for i := 12; i < 20; i += 2 {
		pseudo += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	pseudo += uint32(pkt[9]) + uint32(len(l4))
	return ones(l4, pseudo) == 0
}

func TestNATChecksumBaseline(t *testing.T) {
	p := makeUDP(netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), 1000, 53, []byte("hello"))
	if !ipValid(p) || !udpValid(p) {
		t.Fatal("freshly built packet should have valid checksums")
	}
}

func TestSNATRoundTrip(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{action: snatAction, src: netip.MustParsePrefix("192.168.1.0/24"), to: a("10.0.0.1")}}, 0)

	out := makeUDP(a("192.168.1.5"), a("10.0.0.2"), 1000, 53, []byte("query"))
	nt.translateOut(out)
	_, _, src, _, sport, _, _ := ipv4Fields(out)
	if src != a("10.0.0.1") {
		t.Fatalf("SNAT did not rewrite source: %s", src)
	}
	if !ipValid(out) || !udpValid(out) {
		t.Fatal("checksums invalid after SNAT")
	}

	// Reply to the translated source must reverse to the original host.
	reply := makeUDP(a("10.0.0.2"), a("10.0.0.1"), 53, sport, []byte("answer"))
	nt.translateIn(reply)
	_, _, _, dst, _, dport, _ := ipv4Fields(reply)
	if dst != a("192.168.1.5") || dport != 1000 {
		t.Fatalf("reverse SNAT wrong: dst=%s dport=%d", dst, dport)
	}
	if !ipValid(reply) || !udpValid(reply) {
		t.Fatal("checksums invalid after reverse SNAT")
	}
}

func TestDNATRoundTrip(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{action: dnatAction, dst: netip.MustParsePrefix("10.0.0.1/32"), to: a("192.168.1.5")}}, 0)

	in := makeUDP(a("10.0.0.9"), a("10.0.0.1"), 5000, 80, []byte("req"))
	nt.translateIn(in)
	_, _, _, dst, _, _, _ := ipv4Fields(in)
	if dst != a("192.168.1.5") {
		t.Fatalf("DNAT did not rewrite dest: %s", dst)
	}
	if !ipValid(in) || !udpValid(in) {
		t.Fatal("checksums invalid after DNAT")
	}

	// Reply from the internal host must have its source restored to the gateway.
	reply := makeUDP(a("192.168.1.5"), a("10.0.0.9"), 80, 5000, []byte("resp"))
	nt.translateOut(reply)
	_, _, src, _, _, _, _ := ipv4Fields(reply)
	if src != a("10.0.0.1") {
		t.Fatalf("reverse DNAT wrong: src=%s", src)
	}
}

func TestSNATPortReallocation(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{action: snatAction, to: a("10.0.0.1")}}, 0)

	// Two internal hosts use the same source port to the same destination.
	p1 := makeUDP(a("192.168.1.5"), a("10.0.0.2"), 1111, 53, nil)
	p2 := makeUDP(a("192.168.1.6"), a("10.0.0.2"), 1111, 53, nil)
	nt.translateOut(p1)
	nt.translateOut(p2)
	_, _, _, _, sp1, _, _ := ipv4Fields(p1)
	_, _, _, _, sp2, _, _ := ipv4Fields(p2)
	if sp1 == sp2 {
		t.Fatalf("PAT should give the colliding flow a distinct port (both %d)", sp1)
	}
}

func TestNATMasqueradeEndToEnd(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x4A70)
	a := netip.MustParseAddr

	// A masquerades a "LAN" (192.168.1.0/24) behind its overlay address.
	ks, _ := crypto.NewKeySet([]string{key})
	devA := newFakeDev("A")
	engA := NewEngine(Options{
		NodeID: "A", Hostname: "A",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: devA, Self4: a("10.0.0.1"),
			NATEnabled: true, NAT: []NATRuleSpec{{Direction: "overlay2underlay", Source: "192.168.1.0/24", Translate: "10.0.0.1"}},
		}},
	})
	trA, err := openTestTransport(engA)
	if err != nil {
		t.Fatal(err)
	}
	engA.Attach(trA)
	engA.Start()
	A := &testNode{engA, trA, devA}

	B := spinNode(t, "B", netID, key, a("10.0.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	// A LAN host behind A sends to B; A should masquerade the source.
	A.dev.in <- makeUDP(a("192.168.1.50"), a("10.0.0.2"), 4444, 53, []byte("dns"))

	var tport uint16
	select {
	case got := <-B.dev.out:
		_, _, src, dst, sport, _, _ := ipv4Fields(got)
		if src != a("10.0.0.1") || dst != a("10.0.0.2") {
			t.Fatalf("B saw un-masqueraded packet: src=%s dst=%s", src, dst)
		}
		if !ipValid(got) || !udpValid(got) {
			t.Fatal("masqueraded packet has bad checksums")
		}
		tport = sport
	case <-time.After(5 * time.Second):
		t.Fatal("masqueraded packet never reached B")
	}

	// B replies to the translated source; A must reverse it back to the LAN host.
	B.dev.in <- makeUDP(a("10.0.0.2"), a("10.0.0.1"), 53, tport, []byte("ans"))
	select {
	case got := <-A.dev.out:
		_, _, _, dst, _, dport, _ := ipv4Fields(got)
		if dst != a("192.168.1.50") || dport != 4444 {
			t.Fatalf("reply not reversed to LAN host: dst=%s dport=%d", dst, dport)
		}
		if !ipValid(got) || !udpValid(got) {
			t.Fatal("reversed reply has bad checksums")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reverse-translated reply never reached A's TUN")
	}
}

// TestNATDoesNotMasqueradeSelfOverlaySource verifies the fix for: enabling NAT
// (a broad "masquerade everything" rule, empty Source = any) must not rewrite the
// source of the node's OWN overlay traffic, or managed-mode web admin over the
// tunnel (and any overlay-internal reply path) breaks. Other gatewayed sources
// are still masqueraded.
func TestNATDoesNotMasqueradeSelfOverlaySource(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x4A71)
	a := netip.MustParseAddr

	ks, _ := crypto.NewKeySet([]string{key})
	devA := newFakeDev("A")
	engA := NewEngine(Options{
		NodeID: "A", Hostname: "A",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: devA, Self4: a("10.0.0.1"),
			NATEnabled: true,
			NAT:        []NATRuleSpec{{Direction: "overlay2underlay", Source: "", Translate: "172.16.0.9"}},
		}},
	})
	trA, err := openTestTransport(engA)
	if err != nil {
		t.Fatal(err)
	}
	engA.Attach(trA)
	engA.Start()
	A := &testNode{engA, trA, devA}
	B := spinNode(t, "B", netID, key, a("10.0.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	// 1. Traffic from A's own overlay address must reach B un-masqueraded.
	A.dev.in <- makeUDP(a("10.0.0.1"), a("10.0.0.2"), 5555, 80, []byte("GET /"))
	select {
	case got := <-B.dev.out:
		if _, _, src, _, _, _, _ := ipv4Fields(got); src != a("10.0.0.1") {
			t.Fatalf("self overlay source was masqueraded to %s — breaks managed mode", src)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("self-sourced packet never reached B")
	}

	// 2. A gatewayed (non-self) source is still masqueraded.
	A.dev.in <- makeUDP(a("192.168.1.50"), a("10.0.0.2"), 4444, 53, []byte("dns"))
	select {
	case got := <-B.dev.out:
		if _, _, src, _, _, _, _ := ipv4Fields(got); src != a("172.16.0.9") {
			t.Fatalf("gatewayed source should be masqueraded, got src=%s", src)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("masqueraded packet never reached B")
	}
}
