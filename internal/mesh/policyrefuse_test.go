package mesh

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// v799 and earlier: a partial-mesh spoke re-dialled every other spoke it had an
// endpoint for, once per initLoop tick (1s), forever. The dial was refused by
// design — onHSInit/onHSResp forbid a peer-to-peer link — but the refusal happens
// on *receipt* of the response, which deletes the pending handshake, so
// planHandshake never exhausts its key cycle and never arms seedRetryBackoff.
// That is the same defect v799 fixed on the relay path: the only throttle
// advances solely when a dial goes unanswered.
//
// It survived restarts because the endpoints are reloaded from PeerCache, which
// is folded wholesale into NetSpec.Seeds at construction with no policy filter
// and no node attribution. One field log showed ~150 refusals a minute across
// eight peers, in two windows hours apart.

func policyState(partial, selfSeed bool) (*Engine, *netState) {
	e := &Engine{nodeID: "self", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		byNode:            map[string]*peerSession{},
		nodes:             map[string]*nodeInfo{},
		pending:           map[uint32]*pendingHS{},
		seedOwner:         map[netip.AddrPort]string{},
		seedBackoff:       map[netip.AddrPort]time.Time{},
		policyRefusedNode: map[string]time.Time{},
		policyRefusedEP:   map[netip.AddrPort]time.Time{},
	}
	ns.spec.ID = 0x99
	ns.spec.PartialMesh = partial
	ns.spec.SelfSeed = selfSeed
	return e, ns
}

var spokeEP = netip.MustParseAddrPort("203.0.113.7:51820")

// A PeerCache entry has no owner at cold start, so the first dial has to happen
// — that is how we learn who answers. Every subsequent one must be suppressed.
func TestPolicyRefusalSuppressesFurtherDials(t *testing.T) {
	e, ns := policyState(true, false)
	now := time.Now()

	if e.seedRefusedByPolicy(ns, spokeEP, now) {
		t.Fatal("an unattributed endpoint must be dialled once, or its owner is never learned")
	}
	e.noteSeedPolicyRefused(ns, "other-spoke", spokeEP)
	// The cadence that produced the storm.
	if !e.seedRefusedByPolicy(ns, spokeEP, now.Add(time.Second)) {
		t.Fatal("a dial 1s after a policy refusal must be suppressed; that cadence is the storm")
	}
	if !e.seedRefusedByPolicy(ns, spokeEP, now.Add(policyRefusedTTL-time.Second)) {
		t.Error("the refusal should hold for policyRefusedTTL")
	}
	// The refusal reflects the far node's config, which can change, so the
	// suppression must expire rather than be permanent.
	if e.seedRefusedByPolicy(ns, spokeEP, now.Add(policyRefusedTTL+time.Minute)) {
		t.Error("the refusal must expire, or a peer that later becomes a seed is never reachable again")
	}
}

// The refusal is recorded against the node too, so a roamed peer arriving at a
// new address is still covered.
func TestPolicyRefusalFollowsTheNodeAcrossAddresses(t *testing.T) {
	e, ns := policyState(true, false)
	now := time.Now()
	e.noteSeedPolicyRefused(ns, "other-spoke", spokeEP)

	roamed := netip.MustParseAddrPort("198.51.100.9:51820")
	ns.seedOwner[roamed] = "other-spoke"
	if !e.seedRefusedByPolicy(ns, roamed, now.Add(time.Second)) {
		t.Fatal("a new address for an already-refused node must be suppressed too")
	}
}

// Once a peer's endpoint is attributed and we know from gossip that it is not a
// seed, no dial should be spent at all — not even the first.
func TestPolicyGateIsProactiveForKnownPeers(t *testing.T) {
	e, ns := policyState(true, false)
	ns.seedOwner[spokeEP] = "other-spoke"
	ns.nodes["other-spoke"] = &nodeInfo{nodeID: "other-spoke", selfSeed: false}

	if !e.seedRefusedByPolicy(ns, spokeEP, time.Now()) {
		t.Fatal("a known non-seed peer on a partial mesh should never be dialled")
	}
}

// Seeds, and full-mesh nodes, are unrestricted — the gate must not touch them.
func TestPolicyGateInertForSeedsAndFullMesh(t *testing.T) {
	for _, tc := range []struct {
		name          string
		partial, self bool
	}{
		{"full mesh", false, false},
		{"this node is a seed", true, true},
	} {
		e, ns := policyState(tc.partial, tc.self)
		ns.seedOwner[spokeEP] = "peer"
		ns.nodes["peer"] = &nodeInfo{nodeID: "peer", selfSeed: false}
		e.noteSeedPolicyRefused(ns, "peer", spokeEP)
		if e.seedRefusedByPolicy(ns, spokeEP, time.Now().Add(time.Second)) {
			t.Errorf("%s: every link is permitted here; nothing should be suppressed", tc.name)
		}
	}
}

// Gossip is the fast recovery path: the moment the far node advertises itself as
// a seed the link is permitted, and waiting out policyRefusedTTL would be a
// half-hour outage for a config change that already took effect.
func TestSeedGossipClearsPolicyRefusal(t *testing.T) {
	e, ns := policyState(true, false)
	e.noteSeedPolicyRefused(ns, "now-a-seed", spokeEP)
	ns.seedBackoff[spokeEP] = time.Now().Add(time.Minute)
	if !e.seedRefusedByPolicy(ns, spokeEP, time.Now().Add(time.Second)) {
		t.Fatal("precondition: the refusal should be in effect")
	}

	ns.mu.Lock()
	ns.clearSeedPolicyRefused("now-a-seed")
	ns.mu.Unlock()

	if e.seedRefusedByPolicy(ns, spokeEP, time.Now().Add(2*time.Second)) {
		t.Error("a node advertised as a seed must be dialable immediately")
	}
	ns.mu.Lock()
	_, cooling := ns.seedBackoff[spokeEP]
	ns.mu.Unlock()
	if cooling {
		t.Error("the seed cooldown should be cleared too, so the dial happens on the next tick")
	}
}

// The point of the whole change, stated as a rate: over ten minutes of 1s ticks,
// a forbidden peer should cost a handful of handshakes rather than 600.
func TestForbiddenPeerIsNotDialledEveryTick(t *testing.T) {
	e, ns := policyState(true, false)
	start := time.Now()
	dials := 0
	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += time.Second {
		at := start.Add(elapsed)
		if e.seedRefusedByPolicy(ns, spokeEP, at) {
			continue
		}
		dials++
		// The dial is answered and refused, as it always will be.
		e.noteSeedPolicyRefused(ns, "other-spoke", spokeEP)
	}
	if dials != 1 {
		t.Errorf("dialled %d times in 10 minutes; the unbounded loop managed 600, and inside one "+
			"policyRefusedTTL the correct answer is exactly 1", dials)
	}
}
