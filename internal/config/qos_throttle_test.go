package config

import "testing"

// Enabling QoS must enable the up-throttle (QoS only reorders behind a rate
// cap), seeding a placeholder rate when none is configured.
func TestQoSEnablesUpThrottle(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			QoS: QoS{Enabled: true}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	n := c.Networks[0]
	if !n.Throttle.Enabled {
		t.Fatal("enabling QoS should enable the throttle")
	}
	if n.Throttle.UpBytesPerSec != defaultQoSUpBytesPerSec {
		t.Fatalf("up cap = %d, want placeholder %d", n.Throttle.UpBytesPerSec, defaultQoSUpBytesPerSec)
	}
}

// An already-configured up-rate is preserved, not overwritten by the placeholder.
func TestQoSKeepsExistingUpRate(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			QoS:      QoS{Enabled: true},
			Throttle: Throttle{Enabled: false, UpBytesPerSec: 2_000_000}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	n := c.Networks[0]
	if !n.Throttle.Enabled {
		t.Fatal("enabling QoS should enable the throttle even if a rate was preset")
	}
	if n.Throttle.UpBytesPerSec != 2_000_000 {
		t.Fatalf("up cap = %d, want preserved 2000000", n.Throttle.UpBytesPerSec)
	}
}

// With QoS disabled, the throttle is left entirely alone.
func TestQoSDisabledLeavesThrottle(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].Throttle.Enabled {
		t.Fatal("throttle should stay off when QoS is disabled")
	}
}
