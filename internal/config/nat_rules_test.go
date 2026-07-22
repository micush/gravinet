package config

import "testing"

func natTestCfg() *Config {
	return &Config{
		PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
}

func TestNATRuleAddFull(t *testing.T) {
	c := natTestCfg()
	// SNAT a source subnet toward a dest, translating to a literal address.
	if err := c.NATRuleAdd("lan", "10.0.0.0/24", "203.0.113.0/24", "198.51.100.7", ""); err != nil {
		t.Fatalf("full rule: %v", err)
	}
	r := c.Networks[0].NAT.Rules[0]
	if r.Source != "10.0.0.0/24" || r.Dest != "203.0.113.0/24" || r.Translate != "198.51.100.7" {
		t.Fatalf("rule fields not stored: %+v", r)
	}
	if !c.Networks[0].NAT.Enabled {
		t.Error("adding a rule should enable NAT")
	}
	// masquerade form
	if err := c.NATRuleAdd("lan", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
		t.Fatalf("masquerade: %v", err)
	}
	m := c.Networks[0].NAT.Rules[1]
	if m.Translate != "masquerade" || m.Interface != "eth0" {
		t.Fatalf("masquerade rule wrong: %+v", m)
	}
	// port-forward form: DNAT, no interface — the mode and the target both
	// live in Translate now, there's no separate direction field.
	if err := c.NATRuleAdd("lan", "", "203.0.113.5", "port-forward:10.0.0.9", ""); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	pf := c.Networks[0].NAT.Rules[2]
	if pf.Translate != "port-forward:10.0.0.9" || pf.Interface != "" || pf.Dest != "203.0.113.5" {
		t.Fatalf("port-forward rule wrong: %+v", pf)
	}
}

func TestNATRuleAddRejectsBadInput(t *testing.T) {
	cases := []struct{ src, dst, tr, iface string }{
		{"not-an-ip", "", "masquerade", "eth0"}, // bad source
		{"", "10.0.0.0/24", "masquerade", ""},   // masquerade without iface
		{"", "", "999.1.1.1", ""},               // bad translate
		{"fd00::/8", "", "masquerade", "eth0"},  // IPv6 source
		{"", "", "port-forward:", ""},           // port-forward with no target
		{"", "", "port-forward:not-an-ip", ""},  // port-forward with a bad target
		{"", "", "port-forward:fd00::1", ""},    // port-forward target must be IPv4
	}
	for i, tc := range cases {
		c := natTestCfg()
		if err := c.NATRuleAdd("lan", tc.src, tc.dst, tc.tr, tc.iface); err == nil {
			t.Errorf("case %d (%+v): expected error, got none", i, tc)
		}
	}
}

// TestNATRulePortForwardPrefixCaseInsensitive confirms "Port-Forward:" (or
// any other casing) is recognized the same as the lowercase form the admin
// UI and CLI always write — matters for a hand-edited config file, or one
// migrated from something else.
func TestNATRulePortForwardPrefixCaseInsensitive(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("lan", "", "", "Port-FORWARD:10.0.0.9", ""); err != nil {
		t.Fatalf("mixed-case port-forward prefix should be accepted: %v", err)
	}
	r := c.Networks[0].NAT.Rules[0]
	if r.Translate != "port-forward:10.0.0.9" {
		t.Fatalf("expected the stored form to be normalized to lowercase, got %q", r.Translate)
	}
}

func TestNATRuleDeleteAt(t *testing.T) {
	c := natTestCfg()
	c.NATRuleAdd("lan", "10.0.0.0/24", "", "masquerade", "eth0")
	c.NATRuleAdd("lan", "10.0.0.5/32", "", "198.51.100.9", "")
	if err := c.NATRuleDeleteAt("lan", 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(c.Networks[0].NAT.Rules) != 1 || c.Networks[0].NAT.Rules[0].Translate != "198.51.100.9" {
		t.Fatalf("wrong rule removed: %+v", c.Networks[0].NAT.Rules)
	}
	if err := c.NATRuleDeleteAt("lan", 5); err == nil {
		t.Error("out-of-range delete should error")
	}
}

func TestNATStateTimeoutSet(t *testing.T) {
	c := natTestCfg()
	if err := c.NATStateTimeoutSet(300); err != nil {
		t.Fatalf("set: %v", err)
	}
	if c.NATStateTimeout != 300 {
		t.Errorf("timeout = %d, want 300", c.NATStateTimeout)
	}
	if err := c.NATStateTimeoutSet(999999); err == nil {
		t.Error("out-of-range timeout should error")
	}
}

// Legacy per-network state_timeout must migrate into the global field (largest
// wins) and the per-network fields must be cleared.
func TestNATStateTimeoutMigration(t *testing.T) {
	c := natTestCfg()
	c.Networks[0].NAT.StateTimeout = 240
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.NATStateTimeout != 240 {
		t.Errorf("global timeout = %d, want migrated 240", c.NATStateTimeout)
	}
	if c.Networks[0].NAT.StateTimeout != 0 {
		t.Errorf("per-network timeout = %d, want cleared", c.Networks[0].NAT.StateTimeout)
	}
}

// TestNATRuleDirectionMigration covers NATRule.Direction's retirement (see
// its doc comment): a legacy "underlay2overlay" rule's Translate gets
// "port-forward:" prefixed onto it so it keeps meaning DNAT, while
// "overlay2underlay" and "overlay2overlay" (both always plain SNAT) just
// lose the field. Either way Direction itself is cleared so it's never
// written back out.
func TestNATRuleDirectionMigration(t *testing.T) {
	c := natTestCfg()
	c.Networks[0].NAT.Rules = []NATRule{
		{Direction: "underlay2overlay", Translate: "10.0.0.9", Enabled: true},
		{Direction: "overlay2underlay", Translate: "masquerade", Interface: "eth0", Enabled: true},
		{Direction: "overlay2overlay", Translate: "203.0.113.1", Enabled: true},
		{Direction: "UNDERLAY2OVERLAY", Translate: "10.0.0.5", Enabled: true}, // case-insensitive match
		// The rare DNAT-to-self combination with no clean equivalent: falls
		// back to plain masquerade/SNAT rather than guessing at an address.
		{Direction: "underlay2overlay", Translate: "masquerade", Interface: "eth0", Enabled: true},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rules := c.Networks[0].NAT.Rules
	want := []string{
		"port-forward:10.0.0.9",
		"masquerade",
		"203.0.113.1",
		"port-forward:10.0.0.5",
		"masquerade",
	}
	if len(rules) != len(want) {
		t.Fatalf("expected %d rules to survive migration, got %d: %+v", len(want), len(rules), rules)
	}
	for i, r := range rules {
		if r.Translate != want[i] {
			t.Errorf("rule %d: Translate = %q, want %q", i, r.Translate, want[i])
		}
		if r.Direction != "" {
			t.Errorf("rule %d: Direction = %q, want cleared", i, r.Direction)
		}
	}
}

func TestNATRuleSetEnabled(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("lan", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
		t.Fatal(err)
	}
	// New NAT rules are enabled by default.
	if !c.Networks[0].NAT.Rules[0].Enabled {
		t.Fatal("new NAT rule should be enabled")
	}
	// Disable it by index — the rule stays in config, only the flag flips.
	if err := c.NATRuleSetEnabled("lan", 0, false); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].NAT.Rules) != 1 || c.Networks[0].NAT.Rules[0].Enabled {
		t.Fatalf("rule should be present and disabled: %+v", c.Networks[0].NAT.Rules)
	}
	// Match fields are preserved across the toggle.
	if c.Networks[0].NAT.Rules[0].Interface != "eth0" {
		t.Fatalf("rule fields should be preserved: %+v", c.Networks[0].NAT.Rules[0])
	}
	// Re-enable.
	if err := c.NATRuleSetEnabled("lan", 0, true); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[0].NAT.Rules[0].Enabled {
		t.Fatal("rule should be enabled again")
	}
	// Out-of-range index errors.
	if err := c.NATRuleSetEnabled("lan", 5, false); err == nil {
		t.Error("out-of-range index should error")
	}
	if err := c.NATRuleSetEnabled("lan", -1, false); err == nil {
		t.Error("negative index should error")
	}
}

func TestNATRuleUpdateAt(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("lan", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
		t.Fatal(err)
	}
	if err := c.NATRuleAdd("lan", "10.0.1.0/24", "", "198.51.100.7", ""); err != nil {
		t.Fatal(err)
	}
	// Disable rule 0 so we can confirm the edit preserves state and position.
	if err := c.NATRuleSetEnabled("lan", 0, false); err != nil {
		t.Fatal(err)
	}

	// Edit rule 0: switch from masquerade to port-forward (iface should clear).
	if err := c.NATRuleUpdateAt("lan", 0, "10.0.0.0/24", "203.0.113.0/24", "port-forward:192.0.2.5", "eth0"); err != nil {
		t.Fatal(err)
	}
	r := c.Networks[0].NAT.Rules[0]
	if r.Source != "10.0.0.0/24" || r.Dest != "203.0.113.0/24" {
		t.Fatalf("fields not updated: %+v", r)
	}
	if r.Translate != "port-forward:192.0.2.5" || r.Interface != "" {
		t.Fatalf("port-forward translate should clear iface: %+v", r)
	}
	if r.Enabled {
		t.Fatal("editing a rule must preserve its disabled state")
	}
	// Rule 1 (and overall ordering) is untouched.
	if len(c.Networks[0].NAT.Rules) != 2 || c.Networks[0].NAT.Rules[1].Source != "10.0.1.0/24" {
		t.Fatalf("edit must not reorder/drop rules: %+v", c.Networks[0].NAT.Rules)
	}

	// Edit back to masquerade preserving the (still disabled) state.
	if err := c.NATRuleUpdateAt("lan", 0, "10.0.0.0/24", "", "masquerade", "eth1"); err != nil {
		t.Fatal(err)
	}
	if r := c.Networks[0].NAT.Rules[0]; r.Translate != "masquerade" || r.Interface != "eth1" || r.Enabled {
		t.Fatalf("masquerade edit wrong: %+v", r)
	}

	// Masquerade without an interface is rejected (shared validation with add).
	if err := c.NATRuleUpdateAt("lan", 0, "", "", "masquerade", ""); err == nil {
		t.Error("masquerade without iface should error")
	}
	// Bad port-forward target rejected.
	if err := c.NATRuleUpdateAt("lan", 0, "", "", "port-forward:not-an-ip", ""); err == nil {
		t.Error("bad port-forward target should error")
	}
	// Out-of-range index rejected.
	if err := c.NATRuleUpdateAt("lan", 9, "", "", "masquerade", "eth0"); err == nil {
		t.Error("out-of-range index should error")
	}
}
