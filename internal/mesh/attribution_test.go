package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// v800 added a proactive gate: on a partial mesh, do not dial an address that
// belongs to a node which is not a seed. It could not fire, because the only
// thing that recorded which node an address belongs to was AddSeedFor — and
// learnPeers skips AddSeedFor for exactly those addresses, since calling it
// would also add a dial target. Attribution and intent were the same call.
//
// So a peer-to-peer address that arrived any other way (PeerCache, an operator's
// Seeds list, a post-teardown redial) stayed unattributed and stayed dialable.
// The field cost, from the partial-mesh bundle: eight spokes each dialing one
// other spoke every ~18 seconds indefinitely, ~350 refused handshakes per pair
// in 94 minutes. The responder refuses at onHSInit and answers nothing, so the
// dialer's pending times out, seedRetryBackoff arms (15s), and it retries. v801's
// cooldown made it 18x slower; it could not make it stop, because a silently
// refused dial teaches the dialer nothing.

func attribState() (*Engine, *netState) {
	e, ns := policyState(true, false) // partial mesh, this node is not a seed
	return e, ns
}

// Attribution alone must be enough for the proactive gate — no refusal needed,
// no dial spent learning who owns the address.
func TestAttributionAloneSuppressesForbiddenDial(t *testing.T) {
	e, ns := attribState()
	ep := netip.MustParseAddrPort("203.0.113.50:65432")

	// Unattributed: must be dialable, or its owner is never learned.
	if e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("an unattributed address must stay dialable")
	}

	// Gossip says this address belongs to a non-seed peer. That is sufficient.
	ns.nodes["spoke"] = &nodeInfo{nodeID: "spoke", selfSeed: false}
	e.noteSeedOwnerLocked(ns, ep, "spoke")

	if !e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a known non-seed peer's address must not be dialled on a partial mesh; " +
			"this is the gate v800 added and could never reach")
	}
}

// Recording attribution must never itself create a dial target — that would turn
// a safety mechanism into the storm it exists to prevent.
func TestAttributionDoesNotAddDialTarget(t *testing.T) {
	e, ns := attribState()
	ep := netip.MustParseAddrPort("203.0.113.51:65432")
	before := len(ns.seeds)
	e.noteSeedOwnerLocked(ns, ep, "spoke")
	if len(ns.seeds) != before {
		t.Fatalf("seeds grew from %d to %d: attribution is information, not intent", before, len(ns.seeds))
	}
	if ns.seedOwner[ep] != "spoke" {
		t.Fatalf("seedOwner[%v] = %q, want \"spoke\"", ep, ns.seedOwner[ep])
	}
}

// A seed's address must stay dialable however it was attributed: the whole point
// of a partial mesh is that spokes reach hubs.
func TestAttributedSeedRemainsDialable(t *testing.T) {
	e, ns := attribState()
	ep := netip.MustParseAddrPort("203.0.113.52:65432")
	ns.nodes["hub"] = &nodeInfo{nodeID: "hub", selfSeed: true}
	e.noteSeedOwnerLocked(ns, ep, "hub")
	if e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a hub's address must remain dialable on a partial mesh")
	}
}

// Attribution must not overwrite an existing owner: a wrong owner would suppress
// a legitimate dial, which is worse than a missing one.
func TestAttributionDoesNotOverwriteKnownOwner(t *testing.T) {
	e, ns := attribState()
	ep := netip.MustParseAddrPort("203.0.113.53:65432")
	e.noteSeedOwnerLocked(ns, ep, "first")
	e.noteSeedOwnerLocked(ns, ep, "second")
	if got := ns.seedOwner[ep]; got != "first" {
		t.Errorf("seedOwner = %q, want \"first\": a later claim must not displace an established one", got)
	}
}
