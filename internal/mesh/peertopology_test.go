package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// Complements partialadvertise_test.go, which covers the wire encoding and the
// basic accept/refuse matrix. These are the cases it does not reach: an
// advertised *full* mesh, the relay dial path, and our own setting still being
// enforced independently of the peer's.
//
// config.Network.Mesh is documented as a property of the *network*, but was only
// ever enforced as a property of the node holding it: each node consulted its own
// ns.spec.PartialMesh and nothing else. So setting partial mesh on one node gave
// a node-scoped effect, and the operator's only way to get the documented
// behaviour was to set it everywhere.
//
// Worse, the refusing node was helpless. onHSInit answers nothing when it
// refuses, so a full-mesh dialer learned nothing and retried forever — a field
// bundle showed eight full-mesh nodes doing this to one partial-mesh node at
// ~2230 refusals an hour, indefinitely.
//
// hsPayload.PartialMesh, its flagPartialKnown/flagPartialMesh encoding, and its
// propagation into nodeInfo through gossip all already existed, and the field's
// own doc comment described this exact use: "a full-mesh node can respect a
// partial-mesh peer's topology, which is what makes a mixed fleet behave sanely
// instead of generating refusals forever." Nothing consulted it. These tests pin
// the consumption.

func topoState(ourPartial, ourSeed bool) (*Engine, *netState) {
	e, ns := policyState(ourPartial, ourSeed)
	return e, ns
}

var topoEP = netip.MustParseAddrPort("198.51.100.20:65432")

func addPeer(ns *netState, id string, selfSeed, partialMesh, partialKnown bool) {
	ns.nodes[id] = &nodeInfo{nodeID: id, selfSeed: selfSeed,
		partialMesh: partialMesh, partialKnown: partialKnown}
	ns.seedOwner[topoEP] = id
}

// An advertised full mesh is a positive statement and must not suppress a dial.
func TestPeerAdvertisingFullMeshIsDialed(t *testing.T) {
	e, ns := topoState(false, false)
	addPeer(ns, "full-peer", false, false, true) // knows its mode, says full

	if e.seedRefusedByPolicy(ns, topoEP, time.Now()) {
		t.Fatal("a peer advertising full mesh must be dialed")
	}
}

// The relay path needs the same rule: onHSInit refuses a relayed handshake
// exactly as it refuses a direct one, so a relayed dial to a partial-mesh
// non-seed is equally pointless.
func TestTryRelaysRespectsPartialMeshPeer(t *testing.T) {
	e, ns := stormState(false, false, nil) // we are full mesh, not a seed
	ns.nodes["spoke"] = &nodeInfo{nodeID: "spoke", selfSeed: false, partialMesh: true, partialKnown: true}
	ns.nodes["hub"] = &nodeInfo{nodeID: "hub", selfSeed: true}

	got := wantsOf(e, ns)
	if got["spoke"] {
		t.Error("a relayed handshake to a partial-mesh non-seed is refused on arrival; do not send it")
	}
	if !got["hub"] {
		t.Error("a seed must still be reachable through a relay")
	}
}

// Our own partial-mesh setting must keep working exactly as before — the peer's
// mode is an additional ground for refusal, not a replacement.
func TestOwnPartialMeshStillEnforced(t *testing.T) {
	e, ns := topoState(true, false)          // we are partial mesh, not a seed
	addPeer(ns, "spoke", false, false, true) // peer is full mesh and says so

	if !e.seedRefusedByPolicy(ns, topoEP, time.Now()) {
		t.Fatal("our own partial-mesh config must still forbid a peer-to-peer link")
	}
}
