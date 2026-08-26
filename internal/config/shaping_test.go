package config

import "testing"

// Enabling QoS must enable an up-rate on every mesh interface (QoS only
// reorders behind an egress cap), seeding a placeholder where none is set.
func TestQoSEnablesUpRateOnEveryMeshIface(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{
			{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
			{ID: "5678", Name: "wan", Enabled: true, Subnet4: "10.1.0.0/24", TUNName: "gv9"},
		}}
	c.QoS = QoS{Enabled: true}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, iface := range []string{"mesh0", "gv9"} {
		s := c.ShapingFor(iface)
		if s == nil {
			t.Fatalf("%s got no shaping entry, so QoS has no cap to reorder behind", iface)
		}
		if !s.Enabled {
			t.Errorf("%s: shaping off despite QoS being on", iface)
		}
		if s.UpBytesPerSec != defaultQoSUpBytesPerSec {
			t.Errorf("%s: up = %d, want placeholder %d", iface, s.UpBytesPerSec, defaultQoSUpBytesPerSec)
		}
	}
}

// An already-configured up-rate is preserved, not overwritten by the placeholder.
func TestQoSKeepsExistingUpRate(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
		Shaping:  []IfaceShaping{{Iface: "mesh0", Throttle: Throttle{UpBytesPerSec: 2_000_000}}},
		QoS:      QoS{Enabled: true}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := c.ShapingFor("mesh0")
	if !s.Enabled {
		t.Error("enabling QoS should switch shaping on even if a rate was preset")
	}
	if s.UpBytesPerSec != 2_000_000 {
		t.Errorf("up = %d, want preserved 2000000", s.UpBytesPerSec)
	}
}

// QoS must not reach into an entry the operator wrote for a non-mesh
// interface: QoS does not classify there, and switching on a cap nobody asked
// for is a change to traffic, not a consistency fix.
func TestQoSLeavesNonMeshEntriesAlone(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
		Shaping:  []IfaceShaping{{Iface: "eth0"}},
		QoS:      QoS{Enabled: true}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if s := c.ShapingFor("eth0"); s.Enabled || s.UpBytesPerSec != 0 {
		t.Errorf("QoS seeded a rate onto eth0, which it does not classify: %+v", s.Throttle)
	}
}

// With QoS off, shaping is left entirely alone.
func TestQoSDisabledLeavesShaping(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Shaping) != 0 {
		t.Fatalf("shaping appeared from nowhere with QoS off: %+v", c.Shaping)
	}
}

// The load-bearing migration property: a pre-v960 config comes back shaping
// exactly what it shaped before, under the interface names the shaper was
// already attached to. Different rates on different networks stay different —
// the thing v955 broke and v956 fixed, which must survive this re-keying too.
func TestShapingMigrationKeepsEachNetworksRate(t *testing.T) {
	c := Default()
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 5_000_000}
	c.Networks = []Network{
		{ID: "1111", Name: "slow", Enabled: true, Subnet4: "10.0.0.0/24",
			Throttle: &Throttle{Enabled: true, UpBytesPerSec: 1_000_000}},
		{ID: "2222", Name: "fast", Enabled: true, Subnet4: "10.9.0.0/24",
			Throttle: &Throttle{Enabled: true, UpBytesPerSec: 9_000_000}},
		{ID: "3333", Name: "plain", Enabled: true, Subnet4: "10.8.0.0/24", TUNName: "gv7"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// The two overrides land on their own devices; the network with none
	// takes what the node default was actually giving it.
	for iface, want := range map[string]int{"mesh0": 1_000_000, "mesh1": 9_000_000, "gv7": 5_000_000} {
		s := c.ShapingFor(iface)
		if s == nil {
			t.Fatalf("%s lost its rate in the migration", iface)
		}
		if s.UpBytesPerSec != want {
			t.Errorf("%s: up = %d, want %d", iface, s.UpBytesPerSec, want)
		}
		if !s.Enabled {
			t.Errorf("%s: came back disabled", iface)
		}
	}
	// The legacy fields must not be written back out, or the next load would
	// hoist them a second time over entries the operator has since edited.
	if c.Throttle != (Throttle{}) {
		t.Errorf("the legacy node default survived the hoist: %+v", c.Throttle)
	}
	for _, n := range c.Networks {
		if n.Throttle != nil {
			t.Errorf("%s kept its legacy override", n.Name)
		}
	}
}

// Nothing configured is not a limit: an unshaped node must not come back with
// a row per network saying "disabled, unlimited".
func TestShapingMigrationInventsNothing(t *testing.T) {
	c := Default()
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.Shaping) != 0 {
		t.Fatalf("migration invented entries for an unshaped node: %+v", c.Shaping)
	}
}

// A node default on a node with no networks has no interface to name. It is a
// rate somebody typed, so it is carried to mesh0 — the device the first
// network gets — rather than dropped. Discarding it silently is the v955
// mistake; a visible row is one double-click from being corrected.
func TestShapingMigrationKeepsADefaultWithNoNetworks(t *testing.T) {
	c := Default()
	c.Networks = nil
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 1_250_000, DownBytesPerSec: 1_250_000}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	s := c.ShapingFor("mesh0")
	if s == nil {
		t.Fatal("a configured node default was discarded on a node with no networks")
	}
	if s.UpBytesPerSec != 1_250_000 || s.DownBytesPerSec != 1_250_000 || !s.Enabled {
		t.Errorf("rate not carried across: %+v", s.Throttle)
	}
}

// A config already in the v960 shape is left alone — the hoist must not run a
// second time and overwrite entries the operator has edited since.
func TestShapingMigrationIsIdempotent(t *testing.T) {
	c := Default()
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}
	c.Shaping = []IfaceShaping{{Iface: "mesh0", Throttle: Throttle{Enabled: true, UpBytesPerSec: 3_000_000}}}
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 9_000_000} // stale, from an older write
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.ShapingFor("mesh0").UpBytesPerSec; got != 3_000_000 {
		t.Errorf("up = %d, want the entry's own 3000000 — the legacy default overwrote it", got)
	}
}

// Every entry is enforced by something; which one is decided by the interface.
// A mesh interface is paced in gravinet's own data path, anything else by
// programming the kernel. This is the split that lets a physical NIC be shaped
// at all, so it must not quietly collapse to one side.
func TestShapingKindSplitsMeshFromEverythingElse(t *testing.T) {
	c := Default()
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}
	for _, iface := range []string{"mesh0", "eth0"} {
		if err := c.ShapingAdd(iface); err != nil {
			t.Fatalf("add %s: %v", iface, err)
		}
	}
	if got := c.ShapingKind("mesh0"); got != ShapeTunnel {
		t.Errorf("mesh0 kind = %q, want %q — a qdisc cannot see an overlay packet's class", got, ShapeTunnel)
	}
	if got := c.ShapingKind("eth0"); got != ShapeKernel {
		t.Errorf("eth0 kind = %q, want %q — there is no gravinet data path in front of it", got, ShapeKernel)
	}
}

// KernelShaping is what actually gets programmed. A mesh interface must never
// appear in it: it is already paced in userspace, and a qdisc on top would
// pace the same packets twice.
func TestKernelShapingExcludesMeshInterfaces(t *testing.T) {
	c := Default()
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}
	c.Shaping = []IfaceShaping{
		{Iface: "mesh0", Throttle: Throttle{Enabled: true, UpBytesPerSec: 1_000_000}},
		{Iface: "eth0", Throttle: Throttle{Enabled: true, UpBytesPerSec: 2_000_000}},
	}
	got := c.KernelShaping()
	if len(got) != 1 || got[0].Iface != "eth0" {
		t.Fatalf("KernelShaping() = %+v, want just eth0", got)
	}
}

// A disabled entry is a lifted cap, not a cap at its configured rate, so it
// must not be programmed at all.
func TestKernelShapingSkipsDisabledEntries(t *testing.T) {
	c := Default()
	c.Shaping = []IfaceShaping{{Iface: "eth0", Throttle: Throttle{Enabled: false, UpBytesPerSec: 2_000_000}}}
	if got := c.KernelShaping(); len(got) != 0 {
		t.Errorf("a disabled entry was programmed anyway: %+v", got)
	}
}

// The interface a network's tunnel runs on is TUNName, or mesh<N> from its
// position. Everything keys off this agreeing with the device actually made.
func TestIfaceForNetwork(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "a", Enabled: true, Subnet4: "10.0.0.0/24"},
		{ID: "2222", Name: "b", Enabled: true, Subnet4: "10.1.0.0/24", TUNName: "gv3"},
	}
	if got := c.IfaceForNetwork(c.Networks[0]); got != "mesh0" {
		t.Errorf("auto name = %q, want mesh0", got)
	}
	if got := c.IfaceForNetwork(c.Networks[1]); got != "gv3" {
		t.Errorf("explicit name = %q, want gv3", got)
	}
	// A network this config does not hold must not be given some other
	// network's device by index.
	if got := c.IfaceForNetwork(Network{ID: "9999", Name: "ghost"}); got != "" {
		t.Errorf("unknown network resolved to %q, want empty", got)
	}
}
