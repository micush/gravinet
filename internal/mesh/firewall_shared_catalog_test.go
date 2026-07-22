package mesh

import (
	"net/netip"
	"testing"
)

// TestFirewallCatalogSharedAcrossNetworks proves the address-object/service
// catalog is node-global — set once, usable by every network on the node —
// rather than duplicated per network. A rule on either network referencing
// a name only the OTHER network happened to receive would fail to compile
// (compileRule looks the name up in that network's *firewall.cat and errors
// on an unknown reference — see TestFirewallUnknownObjectRuleSkipped for the
// same mechanism proving the opposite: a name nobody has). Two networks
// here, one SetFirewallObjects/SetFirewallServices call, and a rule on each
// network naming both the object and the service: if the catalog weren't
// actually shared, at least one of the four ReloadFirewallRules calls below
// would return "not a known object"/"unknown service".
func TestFirewallCatalogSharedAcrossNetworks(t *testing.T) {
	e := NewEngine(Options{
		NodeID: "n", Hostname: "n",
		Nets: []NetSpec{
			{ID: 1, Name: "net1", FirewallEnabled: true},
			{ID: 2, Name: "net2", FirewallEnabled: true},
		},
	})

	if err := e.SetFirewallObjects([]FirewallObject{
		{Name: "web", Kind: "host", Addresses: []string{"10.0.0.10"}},
	}); err != nil {
		t.Fatalf("SetFirewallObjects: %v", err)
	}
	if err := e.SetFirewallServices([]FirewallService{
		{Name: "https", Ports: []FirewallServicePort{{Proto: "tcp", PortMin: 443, PortMax: 443}}},
	}); err != nil {
		t.Fatalf("SetFirewallServices: %v", err)
	}

	for _, netID := range []uint64{1, 2} {
		if err := e.ReloadFirewallRules(netID, []FirewallRule{
			{Action: "deny", Dst: "web", Services: []string{"https"}},
		}); err != nil {
			t.Fatalf("net %d: rule referencing shared object/service should compile, got: %v", netID, err)
		}
	}

	// Confirm it's not just that compilation didn't error, but that both
	// networks actually enforce against the same shared definition: denied
	// on both.
	a := netip.MustParseAddr
	for _, netID := range []uint64{1, 2} {
		f, err := e.fwOf(netID)
		if err != nil {
			t.Fatalf("net %d: fwOf: %v", netID, err)
		}
		if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.10"), 6, 443)) {
			t.Errorf("net %d: traffic to the shared 'web' object over the shared 'https' service should be denied", netID)
		}
		if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.11"), 6, 443)) {
			t.Errorf("net %d: traffic outside the shared object should still be allowed", netID)
		}
	}

	// FirewallObjectsList/FirewallServicesList are the single node-global
	// getters (no networkID parameter) the webadmin API and the persist hook
	// read — confirm they see what was just set.
	objs, err := e.FirewallObjectsList()
	if err != nil || len(objs) != 1 || objs[0].Name != "web" {
		t.Fatalf("FirewallObjectsList = %v, %v; want one object named web", objs, err)
	}
	svcs, err := e.FirewallServicesList()
	if err != nil || len(svcs) != 1 || svcs[0].Name != "https" {
		t.Fatalf("FirewallServicesList = %v, %v; want one service named https", svcs, err)
	}
}

// TestFirewallCatalogSeedsNetworkAddedAfterSet proves a network state built
// after SetFirewallObjects has already run starts with the current shared
// catalog rather than empty — the catalog is genuinely node-global state the
// engine hands to every network it builds (at boot or via AddNetwork), not
// just something pushed to whichever networks existed at the time. Builds
// the netState directly (same helper AddNetwork and initial engine startup
// both use) rather than going through the full AddNetwork/Start path, which
// needs keys/a device/transport wiring unrelated to what's being proven here.
func TestFirewallCatalogSeedsNetworkAddedAfterSet(t *testing.T) {
	e := NewEngine(Options{
		NodeID: "n", Hostname: "n",
		Nets: []NetSpec{{ID: 1, Name: "net1", FirewallEnabled: true}},
	})
	if err := e.SetFirewallObjects([]FirewallObject{
		{Name: "web", Kind: "host", Addresses: []string{"10.0.0.10"}},
	}); err != nil {
		t.Fatalf("SetFirewallObjects: %v", err)
	}

	ns := e.newNetState(NetSpec{ID: 2, Name: "net2", FirewallEnabled: true})
	if _, err := compileRule(FirewallRule{Action: "deny", Dst: "web"}, ns.fw.cat); err != nil {
		t.Fatalf("network built after SetFirewallObjects should still see the shared catalog, got: %v", err)
	}
}
