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
	got := qosClassRules(rules)
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
