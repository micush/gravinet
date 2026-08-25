package config

import "testing"

func TestQoSSetClassDSCP(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}

	// Setting a class's mark before any QoS config exists should seed the
	// 5-class default (same as QoSAdd does) rather than erroring.
	if err := c.QoSSetClassDSCP(0, 46); err != nil {
		t.Fatal(err)
	}
	if c.QoS.Classes != 5 {
		t.Fatalf("classes = %d, want seeded default 5", c.QoS.Classes)
	}
	if len(c.QoS.ClassDSCP) < 1 || c.QoS.ClassDSCP[0] != 46 {
		t.Fatalf("class_dscp = %v, want [46, ...]", c.QoS.ClassDSCP)
	}

	// Setting a later class extends the slice, backfilling earlier unset
	// entries with -1 (meaning "no override, use the default") rather than
	// zero (which would be misread as an override to CS0).
	if err := c.QoSSetClassDSCP(3, 0); err != nil {
		t.Fatal(err)
	}
	if len(c.QoS.ClassDSCP) != 4 {
		t.Fatalf("class_dscp length = %d, want 4", len(c.QoS.ClassDSCP))
	}
	for _, cl := range []int{1, 2} {
		if c.QoS.ClassDSCP[cl] != -1 {
			t.Errorf("class %d should be backfilled as -1 (unset), got %d", cl, c.QoS.ClassDSCP[cl])
		}
	}
	if c.QoS.ClassDSCP[3] != 0 {
		t.Fatalf("class 3 = %d, want 0 (CS0 is a valid explicit override)", c.QoS.ClassDSCP[3])
	}

	// Out-of-range class or DSCP value errors without mutating anything.
	if err := c.QoSSetClassDSCP(99, 10); err == nil {
		t.Error("out-of-range class should error")
	}
	if err := c.QoSSetClassDSCP(0, 64); err == nil {
		t.Error("out-of-range dscp (>63) should error")
	}
	if err := c.QoSSetClassDSCP(0, -1); err == nil {
		t.Error("negative dscp should error")
	}
}

func TestQoSClearClassDSCP(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.QoSSetClassDSCP(2, 12); err != nil {
		t.Fatal(err)
	}
	if err := c.QoSClearClassDSCP(2); err != nil {
		t.Fatal(err)
	}
	if got := c.QoS.ClassDSCP[2]; got != -1 {
		t.Fatalf("class 2 after clear = %d, want -1 (reverted to default)", got)
	}
	// Clearing a class with no override, or one that was never touched, errors.
	if err := c.QoSClearClassDSCP(2); err == nil {
		t.Error("clearing an already-cleared override should error")
	}
	if err := c.QoSClearClassDSCP(4); err == nil {
		t.Error("clearing a class with no entry at all should error")
	}
}

// QoS moved from per-network to node-global in v954. A config on disk has the
// old shape, and getting the hoist wrong silently reclassifies traffic.
func TestQoSMigratesFromPerNetwork(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24", QoS: QoS{
			Enabled: true, Classes: 5, DefaultClass: 3, ClassDSCP: []int{46, -1, -1, -1, -1},
			Rules: []QoSRule{{Protocol: "tcp", PortMin: 22, PortMax: 22, Class: 0}},
		}},
		// QoS switched off: its rules come across disabled, the same fold NAT
		// got, because the per-network gate holding them off has no equivalent.
		{ID: "2222", Name: "dmz", Enabled: true, Subnet4: "10.9.0.0/24", QoS: QoS{
			Enabled: false,
			Rules:   []QoSRule{{Protocol: "udp", PortMin: 53, PortMax: 53, Class: 1}},
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v953 QoS config no longer validates: %v", err)
	}
	if len(c.QoS.Rules) != 2 {
		t.Fatalf("want both rules hoisted, got %d: %+v", len(c.QoS.Rules), c.QoS.Rules)
	}
	// Each rule keeps the network it came from, which is what it already
	// meant: classified on that overlay's egress and nowhere else.
	for i, want := range []string{"lan", "dmz"} {
		if c.QoS.Rules[i].Scope != want {
			t.Errorf("rule %d: scope %q, want %q", i, c.QoS.Rules[i].Scope, want)
		}
	}
	if c.QoS.Rules[0].Disabled {
		t.Error("a rule from a QoS-enabled network came across disabled")
	}
	if !c.QoS.Rules[1].Disabled {
		t.Error("a rule from a QoS-disabled network came across enabled — the node is classifying what it did not classify before")
	}
	// Class geometry comes from the first network that had QoS on.
	if c.QoS.Classes != 5 || c.QoS.DefaultClass != 3 || len(c.QoS.ClassDSCP) == 0 || c.QoS.ClassDSCP[0] != 46 {
		t.Errorf("class geometry lost: classes=%d default=%d dscp=%v", c.QoS.Classes, c.QoS.DefaultClass, c.QoS.ClassDSCP)
	}
	for i := range c.Networks {
		if len(c.Networks[i].QoS.Rules) != 0 || c.Networks[i].QoS.Enabled {
			t.Errorf("network %s kept its legacy QoS and will write it back", c.Networks[i].Name)
		}
	}
}

// The point of the change: a node with no mesh network can write a QoS rule.
// A blank scope means every network, so the rule starts working the moment one
// exists — unlike NAT, where blank means the kernel and no overlay at all.
func TestQoSWorksWithNoMeshNetworks(t *testing.T) {
	c := Default()
	c.Networks = nil
	if err := c.QoSAdd("tcp", 22, nil, 0, ""); err != nil {
		t.Fatalf("a node with no mesh networks could not add a QoS rule: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.QoS.Rules) != 1 || !c.QoS.Enabled {
		t.Fatalf("rule not stored: enabled=%v rules=%+v", c.QoS.Enabled, c.QoS.Rules)
	}
	if c.QoS.Rules[0].Scope != "" {
		t.Errorf("scope should default to every network, got %q", c.QoS.Rules[0].Scope)
	}
	if err := c.QoSAdd("tcp", 23, nil, 0, "nope"); err == nil {
		t.Error("a scope naming no mesh network was accepted")
	}
}

// Scope is part of a rule's key. Two rules differing only by scope — the
// ordinary result of hoisting a config with the same rule on two networks —
// must be independently deletable, or removing one silently takes the other.
func TestQoSRulesAreKeyedByScope(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		{ID: "2222", Name: "dmz", Enabled: true, Subnet4: "10.9.0.0/24"},
	}
	for _, sc := range []string{"lan", "dmz"} {
		if err := c.QoSAdd("tcp", 22, nil, 0, sc); err != nil {
			t.Fatalf("add %s: %v", sc, err)
		}
	}
	if err := c.QoSDelete("tcp", 22, nil, "lan"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(c.QoS.Rules) != 1 || c.QoS.Rules[0].Scope != "dmz" {
		t.Fatalf("deleting one scope's rule took the other with it: %+v", c.QoS.Rules)
	}
}
