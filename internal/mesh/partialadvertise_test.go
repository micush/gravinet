package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// Partial-mesh mode is documented as a property of the network
// (config.Network.Mesh) but was only ever read locally: a node consulted its own
// ns.spec.PartialMesh to decide which links it would accept, and told nobody.
//
// So a node switched to partial mesh refused peer-to-peer handshakes its
// full-mesh peers had no way to know would be refused. The refusal answers
// nothing, so the dialer learned nothing and retried forever — a field fleet
// showed ~2230 refusals an hour continuing for as long as the log ran, with the
// refusing node unable to stop it. The operator's only recourse was to set
// partial mesh on all fourteen nodes, which is not what "a network property"
// should mean.
//
// v806 advertises the mode in the handshake and in gossip, so the dialer can
// evaluate the responder's own predicate and decline to dial.

// The mode must survive a handshake round trip, and "not known" must survive it
// too — a peer predating the field must not decode as full mesh.
func TestHandshakePayloadCarriesPartialMesh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
	}{
		{"partial mesh", true},
		{"full mesh", false},
	} {
		in := hsPayload{Index: 1, TimeNano: 2, Ephemeral: make([]byte, 32),
			NodeID: "n", Hostname: "h", PartialMesh: tc.partial}
		out, err := decodeHSPayload(encodeHSPayload(in))
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if !out.PartialKnown {
			t.Errorf("%s: PartialKnown false; this build always states its mode", tc.name)
		}
		if out.PartialMesh != tc.partial {
			t.Errorf("%s: PartialMesh = %v, want %v", tc.name, out.PartialMesh, tc.partial)
		}
	}
}

// The gossip block must round-trip both bits, and must be absent when nobody
// knows anything — the same all-false-costs-nothing shape as every other
// optional block here.
func TestPeerListCarriesPartialMesh(t *testing.T) {
	in := []peerEntry{
		{nodeID: "a", partialKnown: true, partialMesh: true},
		{nodeID: "b", partialKnown: true, partialMesh: false},
		{nodeID: "c"}, // predates the field
	}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range in {
		if out[i].partialKnown != in[i].partialKnown || out[i].partialMesh != in[i].partialMesh {
			t.Errorf("entry %q: got known=%v partial=%v, want known=%v partial=%v",
				in[i].nodeID, out[i].partialKnown, out[i].partialMesh, in[i].partialKnown, in[i].partialMesh)
		}
	}
	none := encodePeerList([]peerEntry{{nodeID: "a"}, {nodeID: "b"}})
	some := encodePeerList([]peerEntry{{nodeID: "a"}, {nodeID: "b", partialKnown: true}})
	if len(some) <= len(none) {
		t.Errorf("the block should only be emitted when a mode is known (%d vs %d bytes)", len(some), len(none))
	}
}

// The headline: a FULL-mesh node declines to dial a partial-mesh non-seed. This
// is what removes the requirement to configure partial mesh everywhere.
func TestFullMeshNodeRespectsPartialMeshPeer(t *testing.T) {
	e, ns := policyState(false, false) // we are full mesh, not a seed
	ep := netip.MustParseAddrPort("203.0.113.70:65432")
	ns.seedOwner[ep] = "spoke"
	ns.nodes["spoke"] = &nodeInfo{nodeID: "spoke", partialKnown: true, partialMesh: true, selfSeed: false}

	if !e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a full-mesh node must not dial a partial-mesh non-seed; the handshake would be " +
			"refused and the refusal teaches the dialer nothing")
	}
}

// A peer predating the field says nothing, and unknown must keep the old
// behaviour rather than have an upgraded node guess — otherwise a rolling
// upgrade silently stops dialing peers that would have accepted.
func TestUnknownModeStillDialed(t *testing.T) {
	e, ns := policyState(false, false)
	ep := netip.MustParseAddrPort("203.0.113.71:65432")
	ns.seedOwner[ep] = "older"
	ns.nodes["older"] = &nodeInfo{nodeID: "older"} // partialKnown false

	if e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a peer whose mode is unknown must still be dialled")
	}
}

// A partial-mesh peer that IS a seed is dialable — that is the permitted
// seed-to-peer link, and suppressing it would cut a spoke off from its hub.
func TestPartialMeshSeedStillDialed(t *testing.T) {
	e, ns := policyState(false, false)
	ep := netip.MustParseAddrPort("203.0.113.72:65432")
	ns.seedOwner[ep] = "hub"
	ns.nodes["hub"] = &nodeInfo{nodeID: "hub", partialKnown: true, partialMesh: true, selfSeed: true}

	if e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a partial-mesh seed must remain dialable; seed-to-peer is a permitted link")
	}
}

// And if WE are a seed, the peer will accept us whatever its mode — the
// responder's predicate has !our.SelfSeed in it.
func TestSeedDialsPartialMeshPeerFreely(t *testing.T) {
	e, ns := policyState(false, true) // we are a seed
	ep := netip.MustParseAddrPort("203.0.113.73:65432")
	ns.seedOwner[ep] = "spoke"
	ns.nodes["spoke"] = &nodeInfo{nodeID: "spoke", partialKnown: true, partialMesh: true}

	if e.seedRefusedByPolicy(ns, ep, time.Now()) {
		t.Fatal("a seed may link with a partial-mesh non-seed; that is seed-to-peer")
	}
}
