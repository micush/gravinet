package mesh

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// v798 and earlier: a partial-mesh spoke attempted a relayed handshake to every
// other spoke it heard about, every maintInterval, forever. The attempt was not
// merely useless — onHSInit/onHSResp refuse a peer-to-peer link by design — it
// was useless *after a round trip*, and the response deleted the pending
// handshake, so relayPendingTTL never throttled anything and the next tick
// started over. One field log carried 7825 partial-mesh rejections and a relayed
// handshake to the same target every 5 seconds without pause.
//
// Two independent fixes, tested separately: tryRelays no longer proposes a link
// the policy forbids, and relayed attempts back off per target regardless of why
// they fail.

func stormState(partial, selfSeed bool, nodes map[string]bool) (*Engine, *netState) {
	e := &Engine{nodeID: "self", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		byNode:        map[string]*peerSession{},
		nodes:         map[string]*nodeInfo{},
		pending:       map[uint32]*pendingHS{},
		relayAttempts: map[string]*relayAttempt{},
		seedBackoff:   map[netip.AddrPort]time.Time{},
	}
	ns.spec.ID = 0x77
	ns.spec.PartialMesh = partial
	ns.spec.SelfSeed = selfSeed
	for id, isSeed := range nodes {
		ns.nodes[id] = &nodeInfo{nodeID: id, selfSeed: isSeed}
	}
	return e, ns
}

// wantsOf reports which targets tryRelays would consider, by observing which
// ones it recorded an attempt against. A target filtered out by policy never
// reaches the attempt bookkeeping.
func wantsOf(e *Engine, ns *netState) map[string]bool {
	e.tryRelays(ns)
	got := map[string]bool{}
	ns.mu.Lock()
	for target := range ns.relayAttempts {
		got[target] = true
	}
	ns.mu.Unlock()
	return got
}

// A spoke must not propose a link to another spoke: the handshake would be
// refused on both sides, so the attempt can never succeed.
func TestTryRelaysSkipsForbiddenPeerToPeerLinks(t *testing.T) {
	e, ns := stormState(true, false, map[string]bool{
		"hub":         true,
		"other-spoke": false,
	})
	got := wantsOf(e, ns)
	if got["other-spoke"] {
		t.Error("a partial-mesh spoke proposed a relayed link to another spoke; policy refuses it every time")
	}
	if !got["hub"] {
		t.Error("a spoke must still reach seeds through a relay")
	}
}

// A seed may link to anything, so nothing is filtered for it.
func TestTryRelaysUnrestrictedForSeedsAndFullMesh(t *testing.T) {
	e, ns := stormState(true, true, map[string]bool{"hub": true, "spoke": false})
	if got := wantsOf(e, ns); !got["spoke"] || !got["hub"] {
		t.Errorf("a seed may link to both seeds and peers; considered %v", got)
	}
	e2, ns2 := stormState(false, false, map[string]bool{"a": false, "b": false})
	if got := wantsOf(e2, ns2); !got["a"] || !got["b"] {
		t.Errorf("full mesh must be unrestricted; considered %v", got)
	}
}

// The backoff bounds the whole class, not just the partial-mesh cause: any
// target that answers and is then rejected frees its pending slot immediately,
// so relayPendingTTL cannot throttle it.
func TestRelayAttemptBackoffGrowsAndCaps(t *testing.T) {
	if got := relayAttemptBackoff(1); got != relayAttemptBase {
		t.Errorf("after one attempt: got %v, want the base %v", got, relayAttemptBase)
	}
	if got := relayAttemptBackoff(2); got != 2*relayAttemptBase {
		t.Errorf("after two: got %v, want %v", got, 2*relayAttemptBase)
	}
	for _, n := range []int{50, 500} {
		if got := relayAttemptBackoff(n); got != relayAttemptMax {
			t.Errorf("n=%d: got %v, want the cap %v", n, got, relayAttemptMax)
		}
	}
	// The reset window must exceed the cap, or a target sitting at the cap
	// resets itself between attempts and never backs off at all.
	if relayAttemptReset <= relayAttemptMax {
		t.Errorf("relayAttemptReset (%v) must exceed relayAttemptMax (%v)", relayAttemptReset, relayAttemptMax)
	}
}

func TestRelayAttemptAllowedThrottlesAndRecovers(t *testing.T) {
	_, ns := stormState(false, false, nil)
	t0 := time.Now()

	if !ns.relayAttemptAllowed("t", t0) {
		t.Fatal("the first attempt must be allowed")
	}
	// maintInterval later — the cadence that produced the storm.
	if ns.relayAttemptAllowed("t", t0.Add(5*time.Second)) {
		t.Fatal("a second attempt 5s later must be throttled; that cadence is the storm")
	}
	if !ns.relayAttemptAllowed("t", t0.Add(relayAttemptBase+time.Second)) {
		t.Fatal("an attempt past the base backoff must be allowed")
	}
	// Now at n=2, so the next gap must be 2*base.
	at := t0.Add(relayAttemptBase + time.Second)
	if ns.relayAttemptAllowed("t", at.Add(relayAttemptBase+time.Second)) {
		t.Fatal("the third attempt must wait 2x the base, not 1x")
	}
	if !ns.relayAttemptAllowed("t", at.Add(2*relayAttemptBase+time.Second)) {
		t.Fatal("the third attempt must be allowed once 2x the base has passed")
	}
	// A long quiet period is a genuinely new situation.
	if !ns.relayAttemptAllowed("t", at.Add(relayAttemptReset+time.Hour)) {
		t.Fatal("after relayAttemptReset of quiet, the counter should be forgotten")
	}
	ns.mu.Lock()
	n := ns.relayAttempts["t"].n
	ns.mu.Unlock()
	if n != 1 {
		t.Errorf("counter after reset = %d, want 1", n)
	}
}

// Bounding the storm at its source: over ten minutes of maintenance ticks, a
// permanently unreachable target must be attempted a handful of times, not 120.
func TestUnreachableTargetIsNotAttemptedEveryTick(t *testing.T) {
	_, ns := stormState(false, false, nil)
	start := time.Now()
	attempts := 0
	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += maintInterval {
		ns.mu.Lock()
		if ns.relayAttemptAllowed("dead", start.Add(elapsed)) {
			attempts++
		}
		ns.mu.Unlock()
	}
	if attempts > 8 {
		t.Errorf("attempted %d times in 10 minutes; the unbounded loop managed %d",
			attempts, int(10*time.Minute/maintInterval))
	}
	if attempts < 3 {
		t.Errorf("attempted only %d times in 10 minutes; backoff should not stop retrying entirely", attempts)
	}
}
