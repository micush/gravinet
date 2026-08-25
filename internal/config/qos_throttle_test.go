package config

import "testing"

// Enabling QoS must enable the up-throttle (QoS only reorders behind a rate
// cap), seeding a placeholder rate when none is configured.
func TestQoSEnablesUpThrottle(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	c.QoS = QoS{Enabled: true}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Throttle.Enabled {
		t.Fatal("enabling QoS should enable the throttle")
	}
	if c.Throttle.UpBytesPerSec != defaultQoSUpBytesPerSec {
		t.Fatalf("up cap = %d, want placeholder %d", c.Throttle.UpBytesPerSec, defaultQoSUpBytesPerSec)
	}
}

// An already-configured up-rate is preserved, not overwritten by the placeholder.
func TestQoSKeepsExistingUpRate(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			QoS: QoS{Enabled: true}}}}
	c.Throttle = Throttle{Enabled: false, UpBytesPerSec: 2_000_000}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Throttle.Enabled {
		t.Fatal("enabling QoS should enable the throttle even if a rate was preset")
	}
	if c.Throttle.UpBytesPerSec != 2_000_000 {
		t.Fatalf("up cap = %d, want preserved 2000000", c.Throttle.UpBytesPerSec)
	}
}

// With QoS disabled, the throttle is left entirely alone.
func TestQoSDisabledLeavesThrottle(t *testing.T) {
	c := &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Throttle.Enabled {
		t.Fatal("throttle should stay off when QoS is disabled")
	}
}

// v955 collapsed per-network bandwidth limits into one node-global rate,
// taking the largest where networks disagreed. That was wrong — Tun1→Tun2 and
// Tun1→Tun3 genuinely carry different rates, and one number cannot hold two.
// v956 restored the per-network limit as an override of the node default.
//
// The load-bearing property: a config with different rates on different
// networks comes back with those rates, unchanged.
func TestThrottleKeepsDifferentRatesPerNetwork(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "slow", Enabled: true, Subnet4: "10.0.0.0/24",
			Throttle: &Throttle{Enabled: true, UpBytesPerSec: 1_000_000}},
		{ID: "2222", Name: "fast", Enabled: true, Subnet4: "10.9.0.0/24",
			Throttle: &Throttle{Enabled: true, UpBytesPerSec: 9_000_000}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for i, want := range []int{1_000_000, 9_000_000} {
		got := c.EffectiveThrottle(c.Networks[i]).UpBytesPerSec
		if got != want {
			t.Errorf("network %s: up = %d, want %d — a per-network rate was lost",
				c.Networks[i].Name, got, want)
		}
	}
}

// A network with no override follows the node default, and the default means
// that rate to each such network rather than a total shared between them.
func TestThrottleDefaultAppliesToEachNetworkWithoutAnOverride(t *testing.T) {
	c := Default()
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 5_000_000}
	c.Networks = []Network{
		{ID: "1111", Name: "a", Enabled: true, Subnet4: "10.0.0.0/24"},
		{ID: "2222", Name: "b", Enabled: true, Subnet4: "10.9.0.0/24"},
		{ID: "3333", Name: "c", Enabled: true, Subnet4: "10.8.0.0/24",
			Throttle: &Throttle{Enabled: true, UpBytesPerSec: 2_000_000}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for i, want := range []int{5_000_000, 5_000_000, 2_000_000} {
		if got := c.EffectiveThrottle(c.Networks[i]).UpBytesPerSec; got != want {
			t.Errorf("network %s: up = %d, want %d", c.Networks[i].Name, got, want)
		}
	}
}

// A node with no mesh network can still set a rate — the reason the node
// default exists at all.
func TestThrottleWorksWithNoMeshNetworks(t *testing.T) {
	c := Default()
	c.Networks = nil
	if err := c.ThrottleSet("", "both", 1_250_000); err != nil {
		t.Fatalf("a node with no mesh networks could not set a rate: %v", err)
	}
	if err := c.ThrottleSetEnabled("", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !c.Throttle.Enabled || c.Throttle.UpBytesPerSec != 1_250_000 || c.Throttle.DownBytesPerSec != 1_250_000 {
		t.Fatalf("rate not stored: %+v", c.Throttle)
	}
}

// Overriding one direction on a network must not drop the other to unlimited:
// a new override starts from the node default rather than from zero.
func TestThrottleOverrideSeedsFromTheNodeDefault(t *testing.T) {
	c := Default()
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 5_000_000, DownBytesPerSec: 8_000_000}
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}
	if err := c.ThrottleSet("lan", "up", 1_000_000); err != nil {
		t.Fatalf("set: %v", err)
	}
	eff := c.EffectiveThrottle(c.Networks[0])
	if eff.UpBytesPerSec != 1_000_000 {
		t.Errorf("up = %d, want the override 1000000", eff.UpBytesPerSec)
	}
	if eff.DownBytesPerSec != 8_000_000 {
		t.Errorf("down = %d, want the node default 8000000 carried into the new override", eff.DownBytesPerSec)
	}
	if !eff.Enabled {
		t.Error("the new override came back disabled despite the default being on")
	}
}

// Clearing an override puts the network back on the node default.
func TestThrottleClearOverride(t *testing.T) {
	c := Default()
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 5_000_000}
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
		Throttle: &Throttle{Enabled: true, UpBytesPerSec: 1_000_000}}}
	if err := c.ThrottleClearOverride("lan"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := c.EffectiveThrottle(c.Networks[0]).UpBytesPerSec; got != 5_000_000 {
		t.Errorf("up = %d, want the node default 5000000 after clearing", got)
	}
	if err := c.ThrottleClearOverride("lan"); err == nil {
		t.Error("clearing an absent override should say so rather than succeed silently")
	}
}

// A zero-valued override is indistinguishable from none and would otherwise
// pin a network to "uncapped" forever, so Validate drops it.
func TestThrottleZeroOverrideBecomesInherit(t *testing.T) {
	c := Default()
	c.Throttle = Throttle{Enabled: true, UpBytesPerSec: 5_000_000}
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
		Throttle: &Throttle{}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.Networks[0].Throttle != nil {
		t.Error("a zero override survived and will shadow the node default")
	}
	if got := c.EffectiveThrottle(c.Networks[0]).UpBytesPerSec; got != 5_000_000 {
		t.Errorf("up = %d, want the node default", got)
	}
}
