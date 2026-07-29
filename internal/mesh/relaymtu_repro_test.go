package mesh

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// The field topology, reduced: a relay sitting between a jumbo-MTU segment and
// a path that can only carry ~1500 bytes. cush2 held 9000 toward its LAN and
// 1473 toward mcfed, and relayed most of the mesh across that boundary.
//
// The existing switchboard can block a link but not constrain one, so it
// cannot express the property that matters here. mtuSwitchboard adds a
// per-link datagram ceiling: anything larger is dropped, exactly as an
// oversized UDP datagram is on a real path that will not carry it.
type mtuSwitchboard struct {
	mu      sync.Mutex
	engines map[netip.AddrPort]*Engine
	limits  map[[2]netip.Addr]int
	dropped map[[2]netip.Addr]int
	lossN   map[[2]netip.Addr]int // drop 1 in N datagrams
	seen    map[[2]netip.Addr]int
}

func newMTUSwitchboard() *mtuSwitchboard {
	return &mtuSwitchboard{
		engines: map[netip.AddrPort]*Engine{},
		limits:  map[[2]netip.Addr]int{},
		dropped: map[[2]netip.Addr]int{},
		lossN:   map[[2]netip.Addr]int{},
		seen:    map[[2]netip.Addr]int{},
	}
}

func (sb *mtuSwitchboard) register(addr netip.AddrPort, e *Engine) {
	sb.mu.Lock()
	sb.engines[addr] = e
	sb.mu.Unlock()
}

// limit caps datagrams in both directions between a and b.
func (sb *mtuSwitchboard) limit(a, b netip.Addr, max int) {
	sb.mu.Lock()
	sb.limits[[2]netip.Addr{a, b}] = max
	sb.limits[[2]netip.Addr{b, a}] = max
	sb.mu.Unlock()
}

// loss drops 1 in every n datagrams in both directions, modelling the real
// leg's reassembly losses.
func (sb *mtuSwitchboard) loss(a, b netip.Addr, n int) {
	sb.mu.Lock()
	sb.lossN[[2]netip.Addr{a, b}] = n
	sb.lossN[[2]netip.Addr{b, a}] = n
	sb.mu.Unlock()
}

func (sb *mtuSwitchboard) dropCount(a, b netip.Addr) int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.dropped[[2]netip.Addr{a, b}]
}

func (sb *mtuSwitchboard) deliver(from, to netip.AddrPort, payload []byte) {
	sb.mu.Lock()
	key := [2]netip.Addr{from.Addr(), to.Addr()}
	if max, ok := sb.limits[key]; ok && len(payload) > max {
		sb.dropped[key]++
		sb.mu.Unlock()
		return // too big for this link, as the wire would do
	}
	if n, ok := sb.lossN[key]; ok && n > 0 {
		sb.seen[key]++
		if sb.seen[key]%n == 0 {
			sb.dropped[key]++
			sb.mu.Unlock()
			return
		}
	}
	e := sb.engines[to]
	sb.mu.Unlock()
	if e == nil {
		return
	}
	cp := append([]byte(nil), payload...)
	go e.OnPacket(cp, from, 4)
}

type mtuSender struct {
	sb   *mtuSwitchboard
	self netip.AddrPort
}

func (s mtuSender) Send(to netip.AddrPort, payload []byte) error {
	s.sb.deliver(s.self, to, payload)
	return nil
}

// TestRelayedPathHonoursTheRelaysOnwardMTU is the reproduction this
// investigation should have started from.
//
// A reaches B only through relay R. The A–R link is jumbo; the R–B link can
// carry 1500 bytes. A sizes its packets to B using the path-MTU it discovered
// end-to-end, and v703 additionally caps that against A's own hop to R — but
// neither term is the binding one. The binding constraint is R's *onward* leg
// to B, which A cannot observe and nothing reports to it.
//
// R then forwards with sealAndSend, which does not fragment (only sendData
// does, and only for innerIP), and discards the send result. So a relayed
// packet larger than R's onward MTU is dropped at R silently: no fragmentation,
// no counter, no feedback to A, and A goes on sizing the next one identically.
//
// The assertion is end-to-end delivery of a packet A believes is within the
// path it was told about.
func TestRelayedPathHonoursTheRelaysOnwardMTU(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x7A11)
	sb := newMTUSwitchboard()

	addrA := netip.MustParseAddrPort("100.65.0.1:1")
	addrB := netip.MustParseAddrPort("100.65.0.2:1")
	addrR := netip.MustParseAddrPort("100.65.0.3:1")

	mk := func(name string, self netip.Addr, allowRelay bool, myAddr netip.AddrPort) (*Engine, *fakeDev) {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets:     []NetSpec{{ID: netID, Name: "r", Keys: ks, Dev: dev, Self4: self, AllowRelay: allowRelay}},
		})
		eng.Attach(mtuSender{sb, myAddr})
		sb.register(myAddr, eng)
		eng.Start()
		return eng, dev
	}

	engA, devA := mk("A", netip.MustParseAddr("10.9.0.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.9.0.2"), false, addrB)
	engR, devR := mk("R", netip.MustParseAddr("10.9.0.3"), true, addrR)
	defer func() {
		devA.Close()
		devB.Close()
		devR.Close()
		for _, e := range []*Engine{engA, engB, engR} {
			e.Stop()
		}
	}()

	// A and B cannot reach each other at all; R's onward leg to B is the
	// narrow one, mirroring cush2's 1473 toward mcfed.
	sb.limit(addrA.Addr(), addrB.Addr(), 0)
	sb.limit(addrR.Addr(), addrB.Addr(), 1500)

	engA.AddSeed(netID, addrR)
	engB.AddSeed(netID, addrR)

	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsOf(engA, netID), "B") && engB.connectedToNode(nsOf(engB, netID), "A")
	}) {
		t.Fatal("relayed session never formed; the reproduction cannot proceed")
	}

	// Size a packet to what A believes the path to B will carry.
	nsA := nsOf(engA, netID)
	nsA.mu.RLock()
	psB := nsA.byNode["B"]
	nsA.mu.RUnlock()
	if psB == nil {
		t.Fatal("no session to B on A")
	}
	per := int(psB.maxFrag.Load())
	if per <= 0 {
		t.Fatal("A published no fragment size for B")
	}
	t.Logf("A believes it may send %d-byte payloads to B; R's onward link carries 1500", per)

	payload := make([]byte, per)
	for i := range payload {
		payload[i] = byte(i)
	}
	pkt := makeIPv4(netip.MustParseAddr("10.9.0.1"), netip.MustParseAddr("10.9.0.2"), payload)
	devA.in <- pkt

	select {
	case <-devB.out:
		// Delivered: either it fitted, or something along the path sized it
		// correctly. Either way the invariant holds.
	case <-time.After(10 * time.Second):
		t.Fatalf("a packet sized to A's own advertised path MTU (%d) never reached B: %d datagram(s) were too large for the relay's onward link and dropped there. The relayed path MTU must be min(sender→relay, relay→destination), and the second term is neither discovered nor reported — sealAndSend does not fragment and onRelay discards the result, so the loss is silent at the relay and invisible at both ends",
			per, sb.dropCount(addrR.Addr(), addrB.Addr()))
	}
}

// TestRelayedSessionSurvivesLossOnTheRelayLeg reproduces the symptom that
// actually matters: relayed sessions that come up, hold for a few minutes, get
// reaped, and come back — while every direct session on the same node stays up
// indefinitely.
//
// The field leg carried 34,634 fragment datagrams with 61 lost reassemblies,
// so the relay hop loses packets at a low but steady rate. A relayed session's
// keepalives ride that same leg. The question this asks is whether ordinary
// loss on the relay hop is enough to starve a relayed session into teardown
// while a direct session experiencing identical loss survives — i.e. whether
// relayed sessions have less margin than direct ones, rather than the path
// being worse.
func TestRelayedSessionSurvivesLossOnTheRelayLeg(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained-stability reproduction; runs for over a minute")
	}
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x7A12)
	sb := newMTUSwitchboard()

	addrA := netip.MustParseAddrPort("100.66.0.1:1")
	addrB := netip.MustParseAddrPort("100.66.0.2:1")
	addrR := netip.MustParseAddrPort("100.66.0.3:1")

	mk := func(name string, self netip.Addr, allowRelay bool, myAddr netip.AddrPort) (*Engine, *fakeDev) {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets:     []NetSpec{{ID: netID, Name: "r", Keys: ks, Dev: dev, Self4: self, AllowRelay: allowRelay}},
		})
		eng.Attach(mtuSender{sb, myAddr})
		sb.register(myAddr, eng)
		eng.Start()
		return eng, dev
	}

	engA, devA := mk("A", netip.MustParseAddr("10.10.0.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.10.0.2"), false, addrB)
	engR, devR := mk("R", netip.MustParseAddr("10.10.0.3"), true, addrR)
	defer func() {
		devA.Close()
		devB.Close()
		devR.Close()
		for _, e := range []*Engine{engA, engB, engR} {
			e.Stop()
		}
	}()

	sb.limit(addrA.Addr(), addrB.Addr(), 0) // no direct path: A-B must relay
	engA.AddSeed(netID, addrR)
	engB.AddSeed(netID, addrR)

	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsOf(engA, netID), "A" /*self-guard*/) == false &&
			engA.connectedToNode(nsOf(engA, netID), "B")
	}) {
		t.Fatal("relayed session never formed")
	}

	// Now introduce steady loss on the relay's leg to B, after the session is
	// established, so this measures survival rather than setup.
	sb.loss(addrR.Addr(), addrB.Addr(), 4) // 25%

	nsA := nsOf(engA, netID)
	firstLoss := time.Now()
	var teardowns int
	var lastSeen bool = true

	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		now := engA.connectedToNode(nsA, "B")
		if lastSeen && !now {
			teardowns++
			t.Logf("relayed session to B torn down %v after loss began", time.Since(firstLoss).Round(time.Second))
		}
		lastSeen = now
	}

	// A direct session between A and R rides the same engine, the same timers
	// and the same loss-free link; if only the relayed one dies, the relayed
	// path has less margin, which is the thing to fix.
	directUp := engA.connectedToNode(nsA, "R")
	t.Logf("after 75s at 25%% loss on the relay leg: relayed teardowns=%d, direct session to R up=%v, drops=%d",
		teardowns, directUp, sb.dropCount(addrR.Addr(), addrB.Addr()))

	if teardowns > 0 {
		t.Fatalf("relayed session to B was torn down %d time(s) under loss that the direct session survived: this is the churn seen in the field, reproduced", teardowns)
	}
}

// TestRelayRehandshakeDoesNotBreakRelayedPeers is the end-to-end form of the
// dangling-relay-pointer bug: A reaches B only through R, R re-handshakes
// (which install() services by replacing its peerSession object outright), and
// A must still be able to reach B afterwards.
//
// Before repointRelayUsers, A's session to B went on pointing at R's orphaned
// session: deliver() sealed to keys and a remoteIdx that R had discarded, R
// dropped the packets with no error and no counter, and B stayed dark until
// A's own peerTimeout reaped the session ~30s later and a fresh handshake
// installed a current pointer. Direct peers were never affected because they
// hold no such pointer — which is precisely the asymmetry observed in the
// field, where every direct peer was healthy and only relayed ones came and
// went.
func TestRelayRehandshakeDoesNotBreakRelayedPeers(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x7A13)
	sb := newMTUSwitchboard()

	addrA := netip.MustParseAddrPort("100.67.0.1:1")
	addrB := netip.MustParseAddrPort("100.67.0.2:1")
	addrR := netip.MustParseAddrPort("100.67.0.3:1")

	mk := func(name string, self netip.Addr, allowRelay bool, myAddr netip.AddrPort) (*Engine, *fakeDev) {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets:     []NetSpec{{ID: netID, Name: "r", Keys: ks, Dev: dev, Self4: self, AllowRelay: allowRelay}},
		})
		eng.Attach(mtuSender{sb, myAddr})
		sb.register(myAddr, eng)
		eng.Start()
		return eng, dev
	}

	engA, devA := mk("A", netip.MustParseAddr("10.11.0.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.11.0.2"), false, addrB)
	engR, devR := mk("R", netip.MustParseAddr("10.11.0.3"), true, addrR)
	defer func() {
		devA.Close()
		devB.Close()
		devR.Close()
		for _, e := range []*Engine{engA, engB, engR} {
			e.Stop()
		}
	}()

	sb.limit(addrA.Addr(), addrB.Addr(), 0) // A and B can only meet through R
	engA.AddSeed(netID, addrR)
	engB.AddSeed(netID, addrR)

	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsOf(engA, netID), "B") && engB.connectedToNode(nsOf(engB, netID), "A")
	}) {
		t.Fatal("relayed session never formed")
	}

	nsA := nsOf(engA, netID)
	nsA.mu.RLock()
	relayBefore := nsA.byNode["R"]
	nsA.mu.RUnlock()

	// Force R to re-handshake with A, replacing A's session object for R.
	engA.AddSeed(netID, addrR)
	if !waitUntil(30*time.Second, func() bool {
		nsA.mu.RLock()
		defer nsA.mu.RUnlock()
		return nsA.byNode["R"] != nil && nsA.byNode["R"] != relayBefore
	}) {
		t.Skip("could not force a relay re-handshake in this environment; the unit tests cover the repointing directly")
	}

	// A's session to B must now be pointing at the live relay session, not the
	// orphan — and traffic must still cross.
	nsA.mu.RLock()
	psB, relayNow := nsA.byNode["B"], nsA.byNode["R"]
	nsA.mu.RUnlock()
	if psB == nil {
		t.Fatal("A lost its session to B outright")
	}
	if got := psB.getRelay(); got != relayNow {
		t.Fatalf("after R re-handshaked, A's session to B still points at the orphaned relay session: every packet to B is now sealed to an index R has discarded and dies there silently")
	}

	pkt := makeIPv4(netip.MustParseAddr("10.11.0.1"), netip.MustParseAddr("10.11.0.2"), []byte("post-rehandshake"))
	devA.in <- pkt
	select {
	case <-devB.out:
	case <-time.After(10 * time.Second):
		t.Fatal("no traffic reached B after the relay re-handshaked: this is the field symptom — a relayed peer that works, goes dark, and returns only when its own timeout forces a fresh handshake")
	}
}
