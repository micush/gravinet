package main

import (
	"testing"

	"gravinet/internal/config"
)

// TestDisabledQoSRuleNotClassified verifies that a disabled QoS rule stays in
// config but is dropped from the rules handed to the classifier, while enabled
// rules (including legacy ones with no disabled field) pass through. This is the
// QoS analogue of the per-rule firewall enable/disable behaviour. It exercises
// qosClassRules directly because the compiled classifier exposes no rule view.
func TestDisabledQoSRuleNotClassified(t *testing.T) {
	rules := []config.QoSRule{
		{Protocol: "tcp", PortMin: 22, PortMax: 22, Class: 0},                    // enabled (zero value)
		{Protocol: "udp", PortMin: 53, PortMax: 53, Class: 1, Disabled: true},    // disabled
		{Protocol: "tcp", PortMin: 443, PortMax: 443, Class: 2, Disabled: false}, // enabled explicitly
	}
	got := qosClassRules(rules, nil, "lan")
	if len(got) != 2 {
		t.Fatalf("expected 2 classifier rules, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		// proto 17 = udp, port 53 was the disabled rule; it must be absent.
		if r.Proto == 17 && r.PortMin == 53 {
			t.Errorf("disabled rule leaked into classifier: %+v", r)
		}
	}
}

// TestQoSClassRulesServices covers resolving a rule's named Services against
// the firewall service catalog: a service with multiple legs expands into one
// ClassRule per leg, a service unions with a literal leg on the same rule
// (both present in the output), and a reference to an unknown service is
// skipped rather than becoming a match-everything catch-all.
func TestQoSClassRulesServices(t *testing.T) {
	svcs := []config.FirewallService{
		{Name: "dns", Ports: []config.FirewallServicePort{
			{Proto: "udp", PortMin: 53, PortMax: 53},
			{Proto: "tcp", PortMin: 53, PortMax: 53},
		}},
	}
	rules := []config.QoSRule{
		// Named service alone: expands to 2 legs (udp/53, tcp/53).
		{Services: []string{"dns"}, Class: 0},
		// Named service unioned with a literal leg: 3 legs total.
		{Protocol: "tcp", PortMin: 22, PortMax: 22, Services: []string{"dns"}, Class: 1},
		// Unknown service and no literal leg: contributes nothing (not a
		// catch-all).
		{Services: []string{"nope"}, Class: 2},
	}
	got := qosClassRules(rules, svcs, "lan")
	if len(got) != 5 {
		t.Fatalf("expected 5 classifier rules (2 + 3 + 0), got %d: %+v", len(got), got)
	}
	countClass := func(class int) int {
		n := 0
		for _, r := range got {
			if r.Class == class {
				n++
			}
		}
		return n
	}
	if n := countClass(0); n != 2 {
		t.Errorf("class 0 (dns alone) should expand to 2 legs, got %d", n)
	}
	if n := countClass(1); n != 3 {
		t.Errorf("class 1 (literal + dns) should expand to 3 legs, got %d", n)
	}
	if n := countClass(2); n != 0 {
		t.Errorf("class 2 (unknown service only) should contribute 0 legs, got %d", n)
	}
}

// TestQoSClassRulesCatchAll pins that a rule with no literal leg and no
// Services still matches everything — the behavior from before Services
// existed, which existing configs (and DSCP-only reclassify rules) rely on.
func TestQoSClassRulesCatchAll(t *testing.T) {
	got := qosClassRules([]config.QoSRule{{Class: 4}}, nil, "lan")
	if len(got) != 1 {
		t.Fatalf("expected 1 catch-all classifier rule, got %d: %+v", len(got), got)
	}
	if got[0].Proto != 0 || got[0].PortMin != 0 || got[0].PortMax != 0 {
		t.Errorf("catch-all rule should have zero proto/port, got %+v", got[0])
	}
}
