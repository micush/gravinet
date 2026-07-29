package mesh

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/logx"
)

// TestTryRelaysWhenNoBackoffEntryExists reproduces the state a node was
// observed wedged in: a peer it had previously been connected to became
// permanently unreachable, while that peer still held its own half of the
// session and kept sending into it.
//
// The trap is that a missing seedBackoff entry is ambiguous. tryRelays read it
// as "direct hasn't been tried yet, keep waiting", but install() deletes the
// backoff entry for an endpoint on a successful connect — and prunes the seed
// that produced it. So after a later teardown the node is left known, with a
// valid advertised endpoint, no session, no backoff entry, and no seed that
// would ever dial it again. No dial means no backoff entry; no backoff entry
// meant no relay. A peer behind symmetric NAT, whose advertised endpoint can
// never work anyway, never escapes on its own.
//
// teardownSessions never notifies the peer (it is a purely local operation),
// which is why only one side falls in: the side that reaped is the side that
// has to re-establish, and it is the side that gets stuck.
func TestTryRelaysWhenNoBackoffEntryExists(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})

	e := &Engine{nodeID: "gn-debian", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		nodes:       map[string]*nodeInfo{},
		byNode:      map[string]*peerSession{},
		pending:     map[uint32]*pendingHS{},
		seedBackoff: map[netip.AddrPort]time.Time{},
	}
	ns.spec.ID = 0x1234
	ns.spec.Keys = ks

	// The unreachable node: known via gossip, endpoint advertised, no session.
	ns.nodes["gn-mcfed"] = &nodeInfo{
		nodeID:   "gn-mcfed",
		endpoint: netip.MustParseAddrPort("69.31.125.162:65432"),
	}
	// Deliberately absent: any ns.seeds entry for that endpoint, and any
	// ns.seedBackoff entry — exactly what install() leaves behind after a
	// successful connect that later tore down.

	// A connected, willing relay that reports knowing the target.
	relay := &peerSession{nodeID: "gn-cush2", net: ns}
	relay.markReported([]string{"gn-mcfed"})
	ns.byNode["gn-cush2"] = relay

	// startRelayHandshake registers the pending handshake before it builds and
	// seals the packet, and sealing needs session crypto this bare fixture has
	// no reason to carry. The assertion here is about the *decision* to relay,
	// which is complete once the pending entry exists, so a panic from the
	// send path below that point is not a failure of what is being tested.
	func() {
		defer func() { _ = recover() }()
		e.tryRelays(ns)
	}()

	// Read without ns.mu: startRelayHandshake panicked while holding it (see
	// above), so the lock is still held by the dead frame and taking it here
	// would deadlock. Single-goroutine test, nothing else touches ns.
	pending := len(ns.pending)
	if pending == 0 {
		t.Fatalf("tryRelays started no relayed handshake to a known, unconnected node with a willing relay available: the absence of a backoff entry was read as \"direct has not demonstrably failed yet\", but nothing will ever dial this endpoint to produce one, so the node stays unreachable indefinitely")
	}
}

// The original intent must survive: while a seed for the endpoint exists and
// is not cooling down, a direct attempt is genuinely pending and relaying
// early would stack a hop for no reason.
func TestTryRelaysWaitsWhileADirectDialIsStillPending(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})

	ep := netip.MustParseAddrPort("69.31.125.162:65432")
	e := &Engine{nodeID: "gn-debian", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		nodes:       map[string]*nodeInfo{},
		byNode:      map[string]*peerSession{},
		pending:     map[uint32]*pendingHS{},
		seedBackoff: map[netip.AddrPort]time.Time{},
		seeds:       []netip.AddrPort{ep}, // initSeedTick will dial this
	}
	ns.spec.ID = 0x1234
	ns.spec.Keys = ks
	ns.nodes["gn-mcfed"] = &nodeInfo{nodeID: "gn-mcfed", endpoint: ep}

	relay := &peerSession{nodeID: "gn-cush2", net: ns}
	relay.markReported([]string{"gn-mcfed"})
	ns.byNode["gn-cush2"] = relay

	func() {
		defer func() { _ = recover() }()
		e.tryRelays(ns)
	}()

	// Read without ns.mu: startRelayHandshake panicked while holding it (see
	// above), so the lock is still held by the dead frame and taking it here
	// would deadlock. Single-goroutine test, nothing else touches ns.
	pending := len(ns.pending)
	if pending != 0 {
		t.Fatalf("tryRelays relayed to a node whose direct seed is queued for dialing and not in backoff: stacking a relay hop before direct has had its chance is what the original gate existed to prevent")
	}
}
