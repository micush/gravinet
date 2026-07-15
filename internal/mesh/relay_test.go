package mesh

import (
	"bytes"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// switchboard is an in-process underlay that routes datagrams between engines
// and can block specific address pairs (to simulate unreachable peers).
type switchboard struct {
	mu      sync.Mutex
	engines map[netip.AddrPort]*Engine
	blocked map[[2]netip.Addr]bool
}

func newSwitchboard() *switchboard {
	return &switchboard{engines: map[netip.AddrPort]*Engine{}, blocked: map[[2]netip.Addr]bool{}}
}
func (sb *switchboard) register(addr netip.AddrPort, e *Engine) {
	sb.mu.Lock()
	sb.engines[addr] = e
	sb.mu.Unlock()
}
func (sb *switchboard) block(a, b netip.Addr) {
	sb.mu.Lock()
	sb.blocked[[2]netip.Addr{a, b}] = true
	sb.blocked[[2]netip.Addr{b, a}] = true
	sb.mu.Unlock()
}
func (sb *switchboard) unblock(a, b netip.Addr) {
	sb.mu.Lock()
	delete(sb.blocked, [2]netip.Addr{a, b})
	delete(sb.blocked, [2]netip.Addr{b, a})
	sb.mu.Unlock()
}
func (sb *switchboard) deliver(from, to netip.AddrPort, payload []byte) {
	sb.mu.Lock()
	if sb.blocked[[2]netip.Addr{from.Addr(), to.Addr()}] {
		sb.mu.Unlock()
		return
	}
	e := sb.engines[to]
	sb.mu.Unlock()
	if e == nil {
		return
	}
	cp := append([]byte(nil), payload...)
	go e.OnPacket(cp, from, 4)
}

type sbSender struct {
	sb   *switchboard
	self netip.AddrPort
}

func (s sbSender) Send(to netip.AddrPort, payload []byte) error {
	s.sb.deliver(s.self, to, payload)
	return nil
}

func TestRelay(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x5E14)
	sb := newSwitchboard()

	addrA := netip.MustParseAddrPort("100.64.0.1:1")
	addrB := netip.MustParseAddrPort("100.64.0.2:1")
	addrR := netip.MustParseAddrPort("100.64.0.3:1")

	mk := func(name string, self netip.Addr, allowRelay bool, myAddr netip.AddrPort) (*Engine, *fakeDev) {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets:     []NetSpec{{ID: netID, Name: "r", Keys: ks, Dev: dev, Self4: self, AllowRelay: allowRelay}},
		})
		eng.Attach(sbSender{sb, myAddr})
		sb.register(myAddr, eng)
		eng.Start()
		return eng, dev
	}

	engA, devA := mk("A", netip.MustParseAddr("10.7.0.1"), false, addrA)
	engB, devB := mk("B", netip.MustParseAddr("10.7.0.2"), false, addrB)
	engR, devR := mk("R", netip.MustParseAddr("10.7.0.3"), true, addrR) // R relays
	defer func() {
		devA.Close()
		devB.Close()
		devR.Close()
		for _, e := range []*Engine{engA, engB, engR} {
			e.Stop()
		}
	}()

	// A and B can each reach R, but not each other.
	sb.block(addrA.Addr(), addrB.Addr())

	// Seed A->R and B->R; A and B discover each other through R's gossip.
	engA.AddSeed(netID, addrR)
	engB.AddSeed(netID, addrR)

	// Wait for A and B to connect to each other (necessarily via relay).
	if !waitUntil(30*time.Second, func() bool {
		return engA.connectedToNode(nsOf(engA, netID), "B") && engB.connectedToNode(nsOf(engB, netID), "A")
	}) {
		t.Fatalf("relayed session did not form: A->B=%v B->A=%v",
			engA.connectedToNode(nsOf(engA, netID), "B"), engB.connectedToNode(nsOf(engB, netID), "A"))
	}

	// Send an overlay packet A->B; it must traverse the relay end-to-end.
	payload := []byte("relayed-end-to-end-payload")
	pkt := makeIPv4(netip.MustParseAddr("10.7.0.1"), netip.MustParseAddr("10.7.0.2"), payload)
	devA.in <- pkt

	select {
	case got := <-devB.out:
		if !bytes.Equal(got, pkt) {
			t.Fatalf("relayed packet differs:\n got=%x\nwant=%x", got, pkt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("packet did not traverse the relay")
	}
	t.Log("packet delivered A->B end-to-end through relay R")
}

func nsOf(e *Engine, id uint64) *netState { return e.network(id) }
