package config

import (
	"encoding/json"
	"testing"
)

func TestQoSRuleSetEnabled(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.QoSAdd("lan", "tcp", 22, nil, 0); err != nil {
		t.Fatal(err)
	}
	// New rules are active by default (zero value of Disabled).
	if c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("new QoS rule should default to enabled")
	}
	// Disable it — the rule stays in config, only the flag flips.
	if err := c.QoSRuleSetEnabled("lan", "tcp", 22, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].QoS.Rules) != 1 || !c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatalf("rule should be present and disabled: %+v", c.Networks[0].QoS.Rules)
	}
	// Re-enable it.
	if err := c.QoSRuleSetEnabled("lan", "tcp", 22, nil, true); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("rule should be enabled again")
	}
	// Proto is matched case-insensitively, like QoSDelete.
	if err := c.QoSRuleSetEnabled("lan", "TCP", 22, nil, false); err != nil {
		t.Fatalf("uppercase proto should match: %v", err)
	}
	if !c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("uppercase proto toggle should have taken effect")
	}
	// Toggling an unknown rule errors.
	if err := c.QoSRuleSetEnabled("lan", "udp", 9999, nil, false); err == nil {
		t.Error("toggling a missing rule should error")
	}
}

// TestQoSRuleServices covers a rule defined by named service(s) instead of a
// literal proto/port — the same catalog references (Config.FirewallServices)
// firewall rules resolve their own Services field against. Add/delete/
// enable-disable are keyed by the services set, order- and case-insensitively.
func TestQoSRuleServices(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.QoSAdd("lan", "", 0, []string{"ssh", "rdp"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].QoS.Rules[0].Services; len(got) != 2 {
		t.Fatalf("services = %v, want [ssh rdp]", got)
	}
	// Keyed order- and case-insensitively.
	if err := c.QoSRuleSetEnabled("lan", "", 0, []string{"RDP", "SSH"}, false); err != nil {
		t.Fatalf("reordered/uppercased services should still match: %v", err)
	}
	if !c.Networks[0].QoS.Rules[0].Disabled {
		t.Fatal("rule should be disabled")
	}
	// A different service set is a different rule, not this one.
	if err := c.QoSRuleSetEnabled("lan", "", 0, []string{"dns"}, true); err == nil {
		t.Error("toggling a rule by an unrelated service set should error")
	}
	if err := c.QoSDelete("lan", "", 0, []string{"ssh", "rdp"}); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].QoS.Rules) != 0 {
		t.Fatalf("rule should be gone: %+v", c.Networks[0].QoS.Rules)
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
