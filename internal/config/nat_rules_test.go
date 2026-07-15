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
	if err := c.NATRuleAdd("lan", "overlay2underlay", "10.0.0.0/24", "203.0.113.0/24", "198.51.100.7", ""); err != nil {
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
	if err := c.NATRuleAdd("lan", "", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
		t.Fatalf("masquerade: %v", err)
	}
	m := c.Networks[0].NAT.Rules[1]
	if m.Translate != "masquerade" || m.Interface != "eth0" || m.Direction != NATOverlayToUnderlay {
		t.Fatalf("masquerade rule wrong: %+v", m)
	}
}

func TestNATRuleAddRejectsBadInput(t *testing.T) {
	cases := []struct{ dir, src, dst, tr, iface string }{
		{"sideways", "", "", "masquerade", "eth0"},  // bad direction
		{"", "not-an-ip", "", "masquerade", "eth0"}, // bad source
		{"", "", "10.0.0.0/24", "masquerade", ""},   // masquerade without iface
		{"", "", "", "999.1.1.1", ""},               // bad translate
		{"", "fd00::/8", "", "masquerade", "eth0"},  // IPv6 source
	}
	for i, tc := range cases {
		c := natTestCfg()
		if err := c.NATRuleAdd("lan", tc.dir, tc.src, tc.dst, tc.tr, tc.iface); err == nil {
			t.Errorf("case %d (%+v): expected error, got none", i, tc)
		}
	}
}

func TestNATRuleDeleteAt(t *testing.T) {
	c := natTestCfg()
	c.NATRuleAdd("lan", "", "10.0.0.0/24", "", "masquerade", "eth0")
	c.NATRuleAdd("lan", "", "10.0.0.5/32", "", "198.51.100.9", "")
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

func TestNATRuleSetEnabled(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("lan", "overlay2underlay", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
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
	if err := c.NATRuleAdd("lan", "overlay2underlay", "10.0.0.0/24", "", "masquerade", "eth0"); err != nil {
		t.Fatal(err)
	}
	if err := c.NATRuleAdd("lan", "overlay2underlay", "10.0.1.0/24", "", "198.51.100.7", ""); err != nil {
		t.Fatal(err)
	}
	// Disable rule 0 so we can confirm the edit preserves state and position.
	if err := c.NATRuleSetEnabled("lan", 0, false); err != nil {
		t.Fatal(err)
	}

	// Edit rule 0: switch from masquerade to a literal IP (iface should clear).
	if err := c.NATRuleUpdateAt("lan", 0, "underlay2overlay", "10.0.0.0/24", "203.0.113.0/24", "192.0.2.5", "eth0"); err != nil {
		t.Fatal(err)
	}
	r := c.Networks[0].NAT.Rules[0]
	if r.Direction != NATUnderlayToOverlay || r.Source != "10.0.0.0/24" || r.Dest != "203.0.113.0/24" {
		t.Fatalf("fields not updated: %+v", r)
	}
	if r.Translate != "192.0.2.5" || r.Interface != "" {
		t.Fatalf("literal translate should clear iface: %+v", r)
	}
	if r.Enabled {
		t.Fatal("editing a rule must preserve its disabled state")
	}
	// Rule 1 (and overall ordering) is untouched.
	if len(c.Networks[0].NAT.Rules) != 2 || c.Networks[0].NAT.Rules[1].Source != "10.0.1.0/24" {
		t.Fatalf("edit must not reorder/drop rules: %+v", c.Networks[0].NAT.Rules)
	}

	// Edit back to masquerade preserving the (still disabled) state.
	if err := c.NATRuleUpdateAt("lan", 0, "overlay2underlay", "10.0.0.0/24", "", "masquerade", "eth1"); err != nil {
		t.Fatal(err)
	}
	if r := c.Networks[0].NAT.Rules[0]; r.Translate != "masquerade" || r.Interface != "eth1" || r.Enabled {
		t.Fatalf("masquerade edit wrong: %+v", r)
	}

	// Masquerade without an interface is rejected (shared validation with add).
	if err := c.NATRuleUpdateAt("lan", 0, "overlay2underlay", "", "", "masquerade", ""); err == nil {
		t.Error("masquerade without iface should error")
	}
	// Bad direction rejected.
	if err := c.NATRuleUpdateAt("lan", 0, "sideways", "", "", "masquerade", "eth0"); err == nil {
		t.Error("bad direction should error")
	}
	// Out-of-range index rejected.
	if err := c.NATRuleUpdateAt("lan", 9, "overlay2underlay", "", "", "masquerade", "eth0"); err == nil {
		t.Error("out-of-range index should error")
	}
}
