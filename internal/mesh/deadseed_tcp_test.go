package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestDeadSeedWithTCPFallbackDoesNotDegrade is
// TestDeadSeedRetryDoesNotDegradeOtherPeers's counterpart with real TCP/TLS
// fallback enabled on both nodes (production always enables this — see
// main.go's cfg.TCPFallbackEnabled()), so I's dead "gn-cush1" seed actually
// exercises ensureFallback -> Dual.DialFallback -> TLSTransport.Dial against
// a target with nothing listening on either UDP or TCP, not just the UDP
// path in isolation.
func TestDeadSeedWithTCPFallbackDoesNotDegrade(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time reproduction; skipped under -short")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0xDEAD7C0)

	type node struct {
		eng *Engine
		dev *fakeDev
		udp *transport.Transport
		tls *transport.TLSTransport
	}
	mk := func(name string, self netip.Addr, tcpPort int) *node {
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name, TCPFallbackPort: tcpPort,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}},
		})
		udp, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatal(err)
		}
		tlsTr, err := transport.OpenTLS(transport.TLSOptions{
			BindAddr: "127.0.0.1", Port: tcpPort, Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatalf("OpenTLS: %v", err)
		}
		eng.Attach(transport.Dual{UDP: udp, TLS: tlsTr})
		eng.Start()
		return &node{eng, dev, udp, tlsTr}
	}

	// Ports 0 for UDP (OS-assigned), but TLS needs a concrete port we can
	// also point the dead seed's fallback attempt at deterministically.
	M := mk("mcfed", netip.MustParseAddr("10.45.0.1"), 32443)
	I := mk("ionos1", netip.MustParseAddr("10.45.0.2"), 32444)
	defer func() {
		for _, n := range []*node{M, I} {
			n.dev.Close()
			n.eng.Stop()
			n.udp.Close()
			n.tls.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	M.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(I.udp.Port())))
	I.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(M.udp.Port())))

	// Dead "gn-cush1": nothing listens on UDP *or* TCP here, so once this
	// seed backs off on UDP, ensureFallback's real TLSTransport.Dial gets
	// exercised against a genuinely closed target, over and over.
	deadSeed := netip.AddrPortFrom(lo, 1)
	I.eng.AddSeed(netID, deadSeed)

	if !waitUntil(15*time.Second, func() bool { return M.eng.PeerCount(netID) >= 1 && I.eng.PeerCount(netID) >= 1 }) {
		t.Fatalf("M and I did not connect: M=%d I=%d", M.eng.PeerCount(netID), I.eng.PeerCount(netID))
	}

	const testDuration = 90 * time.Second
	const pollEvery = 3 * time.Second
	deadline := time.Now().Add(testDuration)
	round := 0
	for time.Now().Before(deadline) {
		round++
		payload := []byte{byte(round), byte(round >> 8), 0xCC, 0xDD}
		pkt := makeIPv4(netip.MustParseAddr("10.45.0.1"), netip.MustParseAddr("10.45.0.2"), payload)

		got := make(chan []byte, 1)
		drain := make(chan struct{})
		go func() {
			select {
			case p := <-I.dev.out:
				got <- p
			case <-drain:
			}
		}()

		select {
		case M.dev.in <- pkt:
		case <-time.After(3 * time.Second):
			close(drain)
			t.Fatalf("round %d: M could not enqueue a packet to I", round)
		}

		select {
		case <-got:
		case <-time.After(5 * time.Second):
			close(drain)
			t.Fatalf("round %d (t+%s): I did not receive M's packet — reproduces the reported degradation with real TCP fallback dialing active", round, testDuration-time.Until(deadline))
		}
		time.Sleep(pollEvery)
	}
	t.Logf("completed %d rounds of M->I delivery over %s with real TCP/TLS fallback dialing against a dead target throughout — no degradation observed", round, testDuration)
}
