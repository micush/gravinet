package ddns

import "testing"

// An SOA whose MNAME is the zone apex — the `@ IN SOA @ ...` idiom, which is
// ordinary in a hand-written reverse zone — names no host and resolves to no
// address. Through v1002 findMaster returned it anyway, so every update was
// dialled at a hostname with no address and failed in the socket layer.
func TestMNAMEThatDoesNotResolveFallsBackToTheAnsweringServer(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal"}}
	server := f.start(t)

	master, zone, err := findMaster("node7.corp.internal", []string{server})
	if err != nil {
		t.Fatalf("findMaster: %v", err)
	}
	if zone != "corp.internal" {
		t.Errorf("zone = %q", zone)
	}
	// The fake's MNAME is ns.example, which it answers an address for, so this
	// is the resolving path.
	if master != "127.0.0.1" {
		t.Errorf("master = %q, want the resolved MNAME address", master)
	}

	// Now a server whose MNAME resolves to nothing.
	g := &fakeDNS{zones: []string{"corp.internal"}, mnameUnresolvable: true}
	server2 := g.start(t)
	master2, _, err := findMaster("node7.corp.internal", []string{server2})
	if err != nil {
		t.Fatalf("findMaster: %v", err)
	}
	if master2 != server2 {
		t.Errorf("master = %q, want the answering server %q — an unresolvable MNAME is not something an update can be sent to", master2, server2)
	}
}
