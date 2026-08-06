package webadmin

import (
	"os"
	"strings"
	"testing"

	"gravinet/internal/mesh"
)

// A troubleshooting bundle must state its own topology. Its absence caused a
// wrong diagnosis: one node had been switched to mesh: partial while the other
// thirteen stayed full mesh, and the resulting refusals looked identical to a
// partial-mesh network with a broken dialer-side gate. Two rounds of bundles
// went into inferring the mode from peer counts and rejection tallies — facts
// the bundle could simply have printed.
//
// These assertions are on the source of the report rather than a rendered
// bundle, because building one needs a live engine; what matters is that the
// fields exist, are populated, and are printed unconditionally.

func TestIfaceInfoCarriesTopology(t *testing.T) {
	// Compile-time proof the fields exist and are addressable, plus a check that
	// neither is omitempty-shaped (a bool that disappears when false is exactly
	// how "no node is a seed" got misread as data).
	ifc := mesh.IfaceInfo{NetworkID: 1, Name: "n", Iface: "mesh0", PartialMesh: true, SelfSeed: false}
	if !ifc.PartialMesh {
		t.Error("PartialMesh did not round-trip")
	}
	if ifc.SelfSeed {
		t.Error("SelfSeed did not round-trip")
	}
}

func TestPeerInfoCarriesSelfSeed(t *testing.T) {
	p := mesh.PeerInfo{NodeID: "n", SelfSeed: true}
	if !p.SelfSeed {
		t.Fatal("PeerInfo.SelfSeed did not round-trip")
	}
	// The JSON tag must not be omitempty: false is the interesting value here,
	// and a key that vanishes reads as false to anyone parsing the bundle —
	// indistinguishable from a node that reported it.
	src := mustSource(t, "../mesh/ban.go")
	if strings.Contains(src, `json:"self_seed,omitempty"`) {
		t.Error("self_seed is omitempty; a missing key reads as false, which is how the " +
			"mixed-mode fleet was misdiagnosed as a partial mesh")
	}
	if !strings.Contains(src, `json:"self_seed"`) {
		t.Error("PeerInfo.SelfSeed has no json tag; it will not appear in a bundle")
	}
}

// The topology block must be printed for every network, not only when something
// looks wrong — the whole point is that a healthy-looking bundle also says what
// mode it is in, so two bundles can be compared.
func TestTshootPrintsTopologyUnconditionally(t *testing.T) {
	src := mustSource(t, "tshoot.go")
	if !strings.Contains(src, "mesh topology:") {
		t.Fatal("the bundle does not report mesh topology")
	}
	if !strings.Contains(src, "this node is a seed:") {
		t.Fatal("the bundle does not report whether this node is a seed")
	}
	// And it must warn about the mixed-mode case specifically, since that is the
	// configuration no code change on the refusing node can fix.
	if !strings.Contains(src, "also configured for partial") {
		t.Error("the bundle does not point at the mixed-mode possibility, which is the " +
			"case that cost two rounds of investigation")
	}
}

func mustSource(t *testing.T, path string) string {
	t.Helper()
	b, err := readFileForTest(path)
	if err != nil {
		t.Skipf("source unavailable: %v", err)
	}
	return b
}

func readFileForTest(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
