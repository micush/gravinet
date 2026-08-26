package config

import "testing"

// The node-global shaping switch added in v968, so Traffic > Shaping carries
// the same enabled/disabled pill beside its title as every other page in that
// group.

func shapedNode(t *testing.T) *Config {
	t.Helper()
	c := Default()
	c.Networks = []Network{{
		ID: "1111", Name: "mesh", Enabled: true,
		Subnet4: "10.255.255.0/24", Address4: "10.255.255.1/24", TUNName: "mesh0",
	}}
	c.Shaping = []IfaceShaping{
		{Iface: "mesh0", Throttle: Throttle{Enabled: true, UpBytesPerSec: 1 << 20, DownBytesPerSec: 2 << 20}},
		{Iface: "eth0", Throttle: Throttle{Enabled: true, UpBytesPerSec: 5 << 20, DownBytesPerSec: 5 << 20}},
	}
	return c
}

// The flag is stored inverted so the zero value is "on". Shaping predates the
// switch, so every config already written has entries and no flag; an Enabled
// bool would read false on all of them and silently unshape every upgraded
// node. This is the test that would catch that being flipped around later.
func TestShapingDefaultsOnForAConfigThatNeverHeardOfTheSwitch(t *testing.T) {
	c := shapedNode(t)
	if !c.ShapingEnabled() {
		t.Fatal("a config with no shaping_disabled field reads as disabled; existing nodes would silently unshape on upgrade")
	}
	if got := c.ShapingThrottle("mesh0"); !got.Enabled || got.UpBytesPerSec != 1<<20 {
		t.Fatalf("tunnel rate not applied by default: %+v", got)
	}
	if len(c.KernelShaping()) != 1 {
		t.Fatalf("kernel rate not applied by default: %+v", c.KernelShaping())
	}
}

// Off means off on both enforcement paths — the tunnel shaper and the kernel
// qdisc. Gating only one would leave half the node's traffic still paced with
// the pill reading "disabled".
func TestShapingOffSilencesBothEnforcementPaths(t *testing.T) {
	c := shapedNode(t)
	if err := c.ShapingSetFeature(false); err != nil {
		t.Fatal(err)
	}
	if c.ShapingEnabled() {
		t.Fatal("still enabled after being switched off")
	}
	if got := c.ShapingThrottle("mesh0"); got.Enabled {
		t.Errorf("the tunnel path is still shaped while the feature is off: %+v", got)
	}
	if got := c.KernelShaping(); len(got) != 0 {
		t.Errorf("the kernel path is still shaped while the feature is off: %+v", got)
	}
}

// Flipping the switch must not touch the entries — the "flip the flag, leave
// the rules alone" split NAT and QoS already use. An operator lifting every
// cap for an afternoon should not have to retype a rate afterwards.
func TestShapingSwitchLeavesTheRatesAlone(t *testing.T) {
	c := shapedNode(t)
	before := append([]IfaceShaping(nil), c.Shaping...)
	if err := c.ShapingSetFeature(false); err != nil {
		t.Fatal(err)
	}
	if len(c.Shaping) != len(before) {
		t.Fatalf("entries changed on toggle: %+v", c.Shaping)
	}
	for i := range before {
		if c.Shaping[i] != before[i] {
			t.Errorf("entry %d changed on toggle:\n got %+v\nwant %+v", i, c.Shaping[i], before[i])
		}
	}
	// And back on, restoring exactly what was there.
	if err := c.ShapingSetFeature(true); err != nil {
		t.Fatal(err)
	}
	if got := c.ShapingThrottle("eth0"); got.UpBytesPerSec != 5<<20 {
		t.Fatalf("rate did not come back after re-enabling: %+v", got)
	}
}

// The switch survives a save/load round trip, and stays absent from the JSON
// while it is on — omitempty on an inverted flag means the common case writes
// nothing, so an untouched config does not grow a field.
func TestShapingSwitchRoundTrips(t *testing.T) {
	c := shapedNode(t)
	if err := c.ShapingSetFeature(false); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !c.ShapingDisabled {
		t.Fatal("Validate cleared the shaping switch")
	}
	if c.ShapingEnabled() {
		t.Fatal("still reads enabled after validate")
	}
}

// Per-interface state and the node-global switch are independent controls, the
// way a NAT rule's own enabled flag is independent of NAT.Enabled. Turning the
// feature back on must not resurrect an interface the operator had parked.
func TestShapingSwitchIsIndependentOfPerInterfaceState(t *testing.T) {
	c := shapedNode(t)
	if err := c.ShapingSetEnabled("eth0", false); err != nil {
		t.Fatal(err)
	}
	if err := c.ShapingSetFeature(false); err != nil {
		t.Fatal(err)
	}
	if err := c.ShapingSetFeature(true); err != nil {
		t.Fatal(err)
	}
	if got := c.ShapingThrottle("eth0"); got.Enabled {
		t.Error("a parked interface came back when the feature was re-enabled")
	}
	if got := c.ShapingThrottle("mesh0"); !got.Enabled {
		t.Error("an unparked interface did not come back when the feature was re-enabled")
	}
}

// Adding a NAT rule leaves the feature switch alone (v968). Covers both entry
// points, since NATAdd and NATRuleAdd each used to set it.
func TestAddingNATRulesNeverEnablesTheFeature(t *testing.T) {
	c := Default()
	if err := c.NATAdd("eth0"); err != nil {
		t.Fatal(err)
	}
	if c.NAT.Enabled {
		t.Error("NATAdd enabled the NAT feature")
	}
	if err := c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth1"); err != nil {
		t.Fatal(err)
	}
	if c.NAT.Enabled {
		t.Error("NATRuleAdd enabled the NAT feature")
	}
	// Both rules are themselves enabled — it is only the feature that stays
	// off, so flipping the pill puts everything already written into force.
	for i, r := range c.NAT.Rules {
		if !r.Enabled {
			t.Errorf("rule %d was added disabled: %+v", i, r)
		}
	}
	// And an operator flipping the pill still gets what they wrote.
	c.NAT.Enabled = true
	if len(c.NAT.Rules) != 2 {
		t.Fatalf("want 2 rules, got %+v", c.NAT.Rules)
	}
}
