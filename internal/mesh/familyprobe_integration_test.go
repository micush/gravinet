package mesh

import (
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestBestRedistOriginsExcludesConfirmedDeadFamily reproduces the exact
// scenario reported: an origin redistributes a v6 default route (::/0) and a
// v4 one (0.0.0.0/0), its session is fully live, but its v6 overlay path has
// been probed and gotten nothing back for longer than familyDeadAfter. v6
// destinations must stop resolving through it — the black hole — while v4
// destinations, whose family was never even probed (let alone confirmed
// dead), are untouched.
func TestBestRedistOriginsExcludesConfirmedDeadFamily(t *testing.T) {
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "exit1", prefix: netip.MustParsePrefix("::/0"), metric: 100, lastSeen: now},
			{origin: "exit1", prefix: netip.MustParsePrefix("0.0.0.0/0"), metric: 100, lastSeen: now},
		},
		"exit1",
	)
	ps := ns.fwd.Load().byNode["exit1"]
	if ps == nil {
		t.Fatal("exit1 missing from byNode")
	}
	longAgo := now.Add(-(familyDeadAfter + time.Second)).UnixNano()
	ps.familyProbeSent6.Store(longAgo) // probed well past the deadline, never replied (familyGood6 stays 0)

	if origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("2001:db8::1")); len(origins) != 0 {
		t.Fatalf("v6 destination should have no usable origin (v6 confirmed dead), got %v", origins)
	}
	if origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("203.0.113.1")); len(origins) != 1 || origins[0] != "exit1" {
		t.Fatalf("v4 destination should still resolve through exit1 (v4 never probed, optimistic default), got %v", origins)
	}
}

// TestBestRedistOriginsRestoresFamilyOnRecentReply is the recovery half of
// the above: once a reply lands (familyGood6 recent), the same origin
// becomes usable for v6 again without needing anything else to change.
func TestBestRedistOriginsRestoresFamilyOnRecentReply(t *testing.T) {
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{{origin: "exit1", prefix: netip.MustParsePrefix("::/0"), metric: 100, lastSeen: now}},
		"exit1",
	)
	ps := ns.fwd.Load().byNode["exit1"]
	ps.familyProbeSent6.Store(now.Add(-1 * time.Hour).UnixNano())
	ps.familyGood6.Store(now.Add(-1 * time.Second).UnixNano())

	if origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("2001:db8::1")); len(origins) != 1 {
		t.Fatalf("v6 destination should resolve through exit1 again (recent reply), got %v", origins)
	}
}

// TestSyncHostsWithholdsOnlyDeadFamily confirms the hosts-file side of the
// same fix: a peer's session is live, its hostname entry is written either
// way, but the specific family confirmed dead is left out of that entry
// rather than either publishing an address nothing can reach or dropping
// the peer's hostname entirely over one broken family.
func TestSyncHostsWithholdsOnlyDeadFamily(t *testing.T) {
	dir := t.TempDir()
	hp := dir + "/hosts"
	if err := os.WriteFile(hp, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
		HostsSync: true, HostsPath: hp,
	}}})
	ns := e.netSnapshot()[1]
	now := time.Now()
	ps := &peerSession{
		net: ns, nodeID: "peer1", hostname: "peer1",
		overlay4: netip.MustParseAddr("10.0.0.9"),
		overlay6: netip.MustParseAddr("fd00::9"),
	}
	ps.familyProbeSent6.Store(now.Add(-(familyDeadAfter + time.Second)).UnixNano()) // v6 confirmed dead; v4 never probed
	ns.mu.Lock()
	ns.byNode["peer1"] = ps
	ns.mu.Unlock()

	e.syncHosts(ns, now)
	got := readFile(t, hp)
	if !strings.Contains(got, "10.0.0.9") {
		t.Errorf("v4 (never probed, optimistic) should still appear:\n%s", got)
	}
	if strings.Contains(got, "fd00::9") {
		t.Errorf("v6 (confirmed dead) should have been withheld:\n%s", got)
	}
}

// TestFamilyProbeRealRoundTrip is the genuine end-to-end check: two real
// engines, real UDP transport, real encryption. A sends a live family probe
// to B; B's dataplane decrypts and delivers it to its own device exactly
// like any other inbound overlay packet (captured here via B's fakeDev
// rather than a real kernel, since nothing in this test environment has a
// real IP stack to auto-reply) — confirms the packet A actually put on the
// wire is a well-formed, correctly-addressed echo request for both v4 and
// v6. The reply is then flipped (flipToEchoReply, the same transformation a
// real OS performs automatically) and sent back through B's own
// processOutbound, so it takes the real return path through the real
// session back to A — confirming deliverInner's recordFamilyProbeReply hook
// actually fires on genuine wire traffic, not just in the pure unit tests
// against hand-built packets.
//
// Seeded one-directionally (A -> B only), deliberately not mirroring
// routes_livereload_test.go's mutual A<->B seeding: two nodes racing to
// connect to each other simultaneously can leave a second, transient
// peerSession reachable via the transport's own inbound dispatch even after
// ns.byNode has moved on to a different session object for the same peer
// (same territory directupgrade_test.go exercises on purpose) — which this
// test would otherwise misreport as "no reply ever recorded" purely because
// it held a *peerSession captured before that resolved, not because
// anything under test was actually broken. One-directional seeding is also
// the more common real topology anyway (a leaf dialing a known peer), so
// this isn't a compromise for the sake of the test.
func TestFamilyProbeRealRoundTrip(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xFA411)
	A := spinNodeDualStack(t, "A", netID, key, netip.MustParseAddr("10.50.0.1"), netip.MustParseAddr("fd50::1"))
	B := spinNodeDualStack(t, "B", netID, key, netip.MustParseAddr("10.50.0.2"), netip.MustParseAddr("fd50::2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	nsA := A.eng.netSnapshot()[netID]
	nsB := B.eng.netSnapshot()[netID]

	A.eng.sendFamilyProbes(nsA)

	// Collect both packets B's dataplane delivered (v4 + v6 echo requests),
	// flip each into a reply, and send it back through B's own outbound
	// pipeline.
	got4, got6 := false, false
	deadline := time.Now().Add(10 * time.Second)
	for (!got4 || !got6) && time.Now().Before(deadline) {
		select {
		case pkt := <-B.dev.out:
			switch {
			case isICMPv4EchoRequestForTest(pkt):
				got4 = true
				B.eng.processOutbound(nsB, flipToEchoReply(pkt))
			case isICMPv6EchoRequestForTest(pkt):
				got6 = true
				B.eng.processOutbound(nsB, flipToEchoReply(pkt))
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !got4 || !got6 {
		t.Fatalf("B never received both probes from A (got4=%v got6=%v)", got4, got6)
	}

	nsA.mu.RLock()
	psAtoB := nsA.byNode["B"]
	nsA.mu.RUnlock()
	if psAtoB == nil {
		t.Fatal("A has no session to B")
	}
	if !waitUntil(5*time.Second, func() bool {
		return psAtoB.familyGood4.Load() != 0 && psAtoB.familyGood6.Load() != 0
	}) {
		t.Fatalf("A never recorded a good reply for both families: v4=%d v6=%d",
			psAtoB.familyGood4.Load(), psAtoB.familyGood6.Load())
	}
	if !familyLive4(psAtoB, time.Now()) || !familyLive6(psAtoB, time.Now()) {
		t.Error("both families should read live after a genuine round trip")
	}
}

func isICMPv4EchoRequestForTest(pkt []byte) bool {
	return len(pkt) >= 21 && pkt[0]>>4 == 4 && pkt[9] == 1 && pkt[20] == 8
}
func isICMPv6EchoRequestForTest(pkt []byte) bool {
	return len(pkt) >= 41 && pkt[0]>>4 == 6 && pkt[6] == 58 && pkt[40] == 128
}

// spinNodeDualStack is spinNode (ban_test.go) plus a Self6 address — needed
// here since spinNode's own signature has no v6 parameter and every other
// test file's use of it is v4-only.
func spinNodeDualStack(t *testing.T, name string, netID uint64, key string, self4, self6 netip.Addr) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets:     []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self4, Self6: self6}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}
