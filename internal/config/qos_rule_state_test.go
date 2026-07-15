package config

import (
	"encoding/json"
	"testing"
)

func TestQoSRuleSetEnabled(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.QoSAdd("lan", "tcp", 22, 0); err != nil {
		t.Fatal(err)
	}
	// New rules are active by default (zero value of Disabled).
	if c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("new QoS rule should default to enabled")
	}
	// Disable it — the rule stays in config, only the flag flips.
	if err := c.QoSRuleSetEnabled("lan", "tcp", 22, false); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].QoS.Rules) != 1 || !c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatalf("rule should be present and disabled: %+v", c.Networks[0].QoS.Rules)
	}
	// Re-enable it.
	if err := c.QoSRuleSetEnabled("lan", "tcp", 22, true); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("rule should be enabled again")
	}
	// Proto is matched case-insensitively, like QoSDelete.
	if err := c.QoSRuleSetEnabled("lan", "TCP", 22, false); err != nil {
		t.Fatalf("uppercase proto should match: %v", err)
	}
	if !c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("uppercase proto toggle should have taken effect")
	}
	// Toggling an unknown rule errors.
	if err := c.QoSRuleSetEnabled("lan", "udp", 9999, false); err == nil {
		t.Error("toggling a missing rule should error")
	}
}

// TestQoSRuleBackwardCompat pins the polarity: a rule written before the
// Disabled field existed must load as enabled, so existing classifiers keep
// working after an upgrade, and an enabled rule omits the key when saved.
func TestQoSRuleBackwardCompat(t *testing.T) {
	var r QoSRule
	if err := json.Unmarshal([]byte(`{"protocol":"tcp","port_min":22,"port_max":22,"class":0}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Disabled {
		t.Fatal("a rule with no disabled field must load as enabled")
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"protocol":"tcp","port_min":22,"port_max":22,"class":0}` {
		t.Fatalf("enabled rule should omit disabled key, got %s", got)
	}
}
