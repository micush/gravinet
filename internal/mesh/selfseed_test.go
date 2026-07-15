package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestSelfSeedDoesNotDegradeOtherPeers goes further than
// TestSelfHandshakeIsRejected (v354), which only proves a single
// self-referential handshake attempt gets rejected. This checks the thing
// that matters after the dead-seed investigation above: does *repeatedly*
// dialing, completing real crypto with, and rejecting a connection to
// itself — over and over, at the normal retry cadence, for several real
// minutes — have any cumulative effect on the same node's ability to talk
// to an unrelated, healthy peer. A self-referential seed is a meaningfully
// different case from a dead third-party seed (TestDeadSeedRetryDoesNot-
// DegradeOtherPeers): it actually completes ephemeral key generation and
// AEAD sealing each cycle before onHSInit's self-check rejects it, rather
// than just timing out silently against nothing.
func TestSelfSeedDoesNotDegradeOtherPeers(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-minute real-time reproduction; skipped under -short")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0x5E1FEED)

	mk := func(name string, self netip.Addr) *testNode {
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}},
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatal(err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}

	M := mk("mcfed", netip.MustParseAddr("10.46.0.1"))
	S := mk("selfseeded", netip.MustParseAddr("10.46.0.2")) // has itself as a seed

	defer func() {
		for _, n := range []*testNode{M, S} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	M.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(S.tr.Port())))
	S.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(M.tr.Port())))
	// The self-referential seed: S dialing its own listening address, over
	// and over, for the whole test.
	S.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(S.tr.Port())))

	if !waitUntil(15*time.Second, func() bool { return M.eng.PeerCount(netID) >= 1 && S.eng.PeerCount(netID) >= 1 }) {
		t.Fatalf("M and S did not connect: M=%d S=%d", M.eng.PeerCount(netID), S.eng.PeerCount(netID))
	}

	const testDuration = 3 * time.Minute
	const pollEvery = 5 * time.Second
	deadline := time.Now().Add(testDuration)
	round := 0
	for time.Now().Before(deadline) {
		round++
		payload := []byte{byte(round), byte(round >> 8), 0xEE, 0xFF}
		pkt := makeIPv4(netip.MustParseAddr("10.46.0.1"), netip.MustParseAddr("10.46.0.2"), payload)

		got := make(chan []byte, 1)
		drain := make(chan struct{})
		go func() {
			select {
			case p := <-S.dev.out:
				got <- p
			case <-drain:
			}
		}()

		select {
		case M.dev.in <- pkt:
		case <-time.After(3 * time.Second):
			close(drain)
			t.Fatalf("round %d: M could not enqueue a packet to S", round)
		}

		select {
		case <-got:
		case <-time.After(5 * time.Second):
			close(drain)
			t.Fatalf("round %d (t+%s): S did not receive M's packet — self-seeding degraded S's ability to talk to an unrelated peer", round, testDuration-time.Until(deadline))
		}

		// S must never actually register a session with itself, the whole
		// test through — not just at t=0.
		for _, pi := range S.eng.ListPeers(netID) {
			if pi.NodeID == "selfseeded" {
				close(drain)
				t.Fatalf("round %d: S has a session with itself in ListPeers", round)
			}
		}
		time.Sleep(pollEvery)
	}
	t.Logf("completed %d rounds of M<->S delivery over %s while S continuously self-seeded — no degradation, no self-session ever formed", round, testDuration)
}
