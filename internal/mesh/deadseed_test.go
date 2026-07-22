package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestDeadSeedRetryDoesNotDegradeOtherPeers reproduces a reported scenario:
// node I (standing in for gn-ionos1) has a seed (standing in for gn-cush1)
// that is genuinely, permanently unreachable — a closed local UDP port, so
// every dial attempt gets no response at all, same as a machine that's
// actually powered off. I is also directly, healthily connected to node M
// (standing in for mcfed). The report: M's connection to I degrades and
// never self-heals — only a restart of I fixes it — while I keeps retrying
// its dead seed in the background.
//
// This drives that exact shape for several real minutes: M sends a data
// packet to I and confirms receipt, repeatedly, while I's initLoop is
// continuously, uselessly retrying the dead seed at its normal cadence
// (handshakeRetry every 2s within an attempt, seedRetryBackoff every 15s
// between attempts) the whole time. If retrying a dead seed has any
// degrading effect on I's ability to process unrelated traffic — a stuck
// lock, a goroutine leak, anything — this should catch it as a mid-test
// failure, not just a final one.
func TestDeadSeedRetryDoesNotDegradeOtherPeers(t *testing.T) {
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
	const netID = uint64(0xDEAD5EED)

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

	M := mk("mcfed", netip.MustParseAddr("10.44.0.1"))
	I := mk("ionos1", netip.MustParseAddr("10.44.0.2"))
	defer func() {
		for _, n := range []*testNode{M, I} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	M.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(I.tr.Port())))
	I.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(M.tr.Port())))

	// A closed local UDP port: sending here gets no response at all — no
	// ICMP surfaces to an unconnected UDP socket, no RST, nothing — the
	// same wire-level silence as a genuinely powered-off machine. This is
	// I's "gn-cush1" seed: never expected to succeed, retried forever.
	deadSeed := netip.AddrPortFrom(lo, 1) // port 1: essentially guaranteed unbound
	I.eng.AddSeed(netID, deadSeed)

	if !waitUntil(15*time.Second, func() bool { return M.eng.PeerCount(netID) >= 1 && I.eng.PeerCount(netID) >= 1 }) {
		t.Fatalf("M and I did not connect: M=%d I=%d", M.eng.PeerCount(netID), I.eng.PeerCount(netID))
	}

	// Confirm the dead seed is actually being retried throughout, not
	// silently given up on — ns.pending or ns.seedBackoff should show
	// activity for it at some point in the loop below; tracked via a log
	// scrape would be fragile, so instead this just runs long enough
	// (several seedRetryBackoff cycles) that retries are certain to have
	// happened repeatedly regardless of exact timing.
	const testDuration = 3 * time.Minute
	const pollEvery = 5 * time.Second
	deadline := time.Now().Add(testDuration)
	round := 0
	for time.Now().Before(deadline) {
		round++
		payload := []byte{byte(round), byte(round >> 8), 0xAA, 0xBB}
		pkt := makeIPv4(netip.MustParseAddr("10.44.0.1"), netip.MustParseAddr("10.44.0.2"), payload)

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
			t.Fatalf("round %d: M could not even enqueue a packet to send to I (M's own tunLoop stalled?)", round)
		}

		select {
		case <-got:
			// delivered — good, continue
		case <-time.After(5 * time.Second):
			close(drain)
			t.Fatalf("round %d (t+%s into the test): I did not receive M's packet — reproduces the reported degradation while I's dead-seed retries are ongoing", round, testDuration-time.Until(deadline))
		}
		time.Sleep(pollEvery)
	}
	t.Logf("completed %d rounds of M->I delivery over %s while I continuously retried a dead seed — no degradation observed", round, testDuration)
}
