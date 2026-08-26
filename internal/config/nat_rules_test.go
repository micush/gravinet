package config

import "testing"

func natTestCfg() *Config {
	return &Config{
		UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
}

func TestNATRuleAddFull(t *testing.T) {
	c := natTestCfg()
	// SNAT a source subnet toward a dest, translating to a literal address.
	if err := c.NATRuleAdd("10.0.0.0/24", "203.0.113.0/24", "", "", "198.51.100.7", ""); err != nil {
		t.Fatalf("full rule: %v", err)
	}
	r := c.NAT.Rules[0]
	if r.Source != "10.0.0.0/24" || r.Dest != "203.0.113.0/24" || r.Translate != "198.51.100.7" {
		t.Fatalf("rule fields not stored: %+v", r)
	}
	// As of v968 adding a rule does NOT switch the feature on: the pill beside
	// the page title is the operator's to flip, the way it already is on QoS
	// and Firewall. Writing a first rule used to put NAT into force node-wide
	// as a side effect.
	if c.NAT.Enabled {
		t.Error("adding a rule enabled NAT; the feature switch is the operator's to flip")
	}
	if !r.Enabled {
		t.Error("the rule itself should be enabled — it is the feature that stays off")
	}
	// masquerade form
	if err := c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth0"); err != nil {
		t.Fatalf("masquerade: %v", err)
	}
	m := c.NAT.Rules[1]
	if m.Translate != "masquerade" || m.Interface != "eth0" {
		t.Fatalf("masquerade rule wrong: %+v", m)
	}
	// port-forward form: DNAT, no interface — the mode and the target both
	// live in Translate now, there's no separate direction field.
	if err := c.NATRuleAdd("", "203.0.113.5", "", "", "port-forward:10.0.0.9", ""); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	pf := c.NAT.Rules[2]
	if pf.Translate != "port-forward:10.0.0.9" || pf.Interface != "" || pf.Dest != "203.0.113.5" {
		t.Fatalf("port-forward rule wrong: %+v", pf)
	}
}

func TestNATRuleAddRejectsBadInput(t *testing.T) {
	cases := []struct{ src, dst, destPort, proto, tr, iface string }{
		{"not-an-ip", "", "", "", "masquerade", "eth0"},                // bad source
		{"", "10.0.0.0/24", "", "", "masquerade", ""},                  // masquerade without iface
		{"", "", "", "", "999.1.1.1", ""},                              // bad translate
		{"fd00::/8", "", "", "", "192.168.1.1", ""},                    // v6 source with v4 target: families must agree
		{"", "", "", "", "port-forward:", ""},                          // port-forward with no target
		{"", "", "", "", "port-forward:not-an-ip", ""},                 // port-forward with a bad target
		{"", "", "", "", "port-forward:[fd00::1", ""},                  // port-forward target with an unclosed bracket
		{"", "", "32400", "", "port-forward:10.0.0.9", ""},             // dest-port without proto
		{"", "", "abc", "tcp", "port-forward:10.0.0.9", ""},            // unparseable dest-port
		{"", "", "0", "tcp", "port-forward:10.0.0.9", ""},              // dest-port out of range
		{"", "", "100-50", "tcp", "port-forward:10.0.0.9", ""},         // inverted range
		{"", "", "32400", "sctp", "port-forward:10.0.0.9", ""},         // bad proto
		{"", "", "32400", "tcp", "masquerade", "eth0"},                 // dest-port on a non-port-forward rule
		{"", "", "8000-8010", "tcp", "port-forward:10.0.0.9:8000", ""}, // remap needs a single port, not a range
		{"", "", "", "", "port-forward:10.0.0.9:notaport", ""},         // bad remap port
		{"", "", "", "", "port-forward:10.0.0.9:99999", ""},            // remap port out of range
	}
	for i, tc := range cases {
		c := natTestCfg()
		if err := c.NATRuleAdd(tc.src, tc.dst, tc.destPort, tc.proto, tc.tr, tc.iface); err == nil {
			t.Errorf("case %d (%+v): expected error, got none", i, tc)
		}
	}
}

// TestNATRuleProtoOnlyWithoutDestPortIsValid checks that Proto can scope a
// port-forward rule to a protocol with no specific port — a meaningful,
// intentional shape (e.g. "forward all tcp traffic to 203.0.113.5,
// regardless of port"), not an error. Only the reverse (a DestPort with no
// Proto) is rejected, since a port only means something for one specific
// protocol's port space.
func TestNATRuleProtoOnlyWithoutDestPortIsValid(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("", "203.0.113.5", "", "tcp", "port-forward:10.0.0.9", ""); err != nil {
		t.Fatalf("proto with no dest-port should be accepted: %v", err)
	}
	if r := c.NAT.Rules[0]; r.Proto != "tcp" || r.DestPort != "" {
		t.Fatalf("rule wrong: %+v", r)
	}
}

// TestNATRulePortForwardPrefixCaseInsensitive confirms "Port-Forward:" (or
// any other casing) is recognized the same as the lowercase form the admin
// UI and CLI always write — matters for a hand-edited config file, or one
// migrated from something else.
func TestNATRulePortForwardPrefixCaseInsensitive(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("", "", "", "", "Port-FORWARD:10.0.0.9", ""); err != nil {
		t.Fatalf("mixed-case port-forward prefix should be accepted: %v", err)
	}
	r := c.NAT.Rules[0]
	if r.Translate != "port-forward:10.0.0.9" {
		t.Fatalf("expected the stored form to be normalized to lowercase, got %q", r.Translate)
	}
}

// TestNATRuleDestPortAndProto covers the PAT-specific fields: a
// port-forward rule can be scoped to a single port or a range, and can
// optionally remap the port on a single-port match.
func TestNATRuleDestPortAndProto(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("", "203.0.113.5", "32400", "tcp", "port-forward:10.0.0.5", ""); err != nil {
		t.Fatalf("single port: %v", err)
	}
	r := c.NAT.Rules[0]
	if r.DestPort != "32400" || r.Proto != "tcp" || r.Translate != "port-forward:10.0.0.5" {
		t.Fatalf("single-port rule wrong: %+v", r)
	}

	if err := c.NATRuleAdd("", "203.0.113.5", "8000-8010", "udp", "port-forward:10.0.0.6", ""); err != nil {
		t.Fatalf("range: %v", err)
	}
	rng := c.NAT.Rules[1]
	if rng.DestPort != "8000-8010" || rng.Proto != "udp" {
		t.Fatalf("range rule wrong: %+v", rng)
	}

	// Remap: a single dest-port with an explicit target port in Translate.
	if err := c.NATRuleAdd("", "203.0.113.5", "8443", "tcp", "port-forward:10.0.0.7:443", ""); err != nil {
		t.Fatalf("remap: %v", err)
	}
	remap := c.NAT.Rules[2]
	if remap.Translate != "port-forward:10.0.0.7:443" || remap.DestPort != "8443" {
		t.Fatalf("remap rule wrong: %+v", remap)
	}

	// Proto is normalized to lowercase.
	if err := c.NATRuleAdd("", "203.0.113.5", "53", "UDP", "port-forward:10.0.0.8", ""); err != nil {
		t.Fatalf("uppercase proto: %v", err)
	}
	if got := c.NAT.Rules[3].Proto; got != "udp" {
		t.Errorf("proto normalization: got %q, want \"udp\"", got)
	}
}

func TestNATRuleDeleteAt(t *testing.T) {
	c := natTestCfg()
	c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth0")
	c.NATRuleAdd("10.0.0.5/32", "", "", "", "198.51.100.9", "")
	if err := c.NATRuleDeleteAt(0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(c.NAT.Rules) != 1 || c.NAT.Rules[0].Translate != "198.51.100.9" {
		t.Fatalf("wrong rule removed: %+v", c.NAT.Rules)
	}
	if err := c.NATRuleDeleteAt(5); err == nil {
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
	c.NAT.Rules = []NATRule{
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
	rules := c.NAT.Rules
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
	if err := c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth0"); err != nil {
		t.Fatal(err)
	}
	// New NAT rules are enabled by default.
	if !c.NAT.Rules[0].Enabled {
		t.Fatal("new NAT rule should be enabled")
	}
	// Disable it by index — the rule stays in config, only the flag flips.
	if err := c.NATRuleSetEnabled(0, false); err != nil {
		t.Fatal(err)
	}
	if len(c.NAT.Rules) != 1 || c.NAT.Rules[0].Enabled {
		t.Fatalf("rule should be present and disabled: %+v", c.NAT.Rules)
	}
	// Match fields are preserved across the toggle.
	if c.NAT.Rules[0].Interface != "eth0" {
		t.Fatalf("rule fields should be preserved: %+v", c.NAT.Rules[0])
	}
	// Re-enable.
	if err := c.NATRuleSetEnabled(0, true); err != nil {
		t.Fatal(err)
	}
	if !c.NAT.Rules[0].Enabled {
		t.Fatal("rule should be enabled again")
	}
	// Out-of-range index errors.
	if err := c.NATRuleSetEnabled(5, false); err == nil {
		t.Error("out-of-range index should error")
	}
	if err := c.NATRuleSetEnabled(-1, false); err == nil {
		t.Error("negative index should error")
	}
}

func TestNATRuleUpdateAt(t *testing.T) {
	c := natTestCfg()
	if err := c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth0"); err != nil {
		t.Fatal(err)
	}
	if err := c.NATRuleAdd("10.0.1.0/24", "", "", "", "198.51.100.7", ""); err != nil {
		t.Fatal(err)
	}
	// Disable rule 0 so we can confirm the edit preserves state and position.
	if err := c.NATRuleSetEnabled(0, false); err != nil {
		t.Fatal(err)
	}

	// Edit rule 0: switch from masquerade to port-forward (iface should clear).
	if err := c.NATRuleUpdateAt(0, "10.0.0.0/24", "203.0.113.0/24", "", "", "port-forward:192.0.2.5", "eth0"); err != nil {
		t.Fatal(err)
	}
	r := c.NAT.Rules[0]
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
	if len(c.NAT.Rules) != 2 || c.NAT.Rules[1].Source != "10.0.1.0/24" {
		t.Fatalf("edit must not reorder/drop rules: %+v", c.NAT.Rules)
	}

	// Add dest-port/proto on the edit.
	if err := c.NATRuleUpdateAt(0, "10.0.0.0/24", "203.0.113.0/24", "32400", "tcp", "port-forward:192.0.2.5", ""); err != nil {
		t.Fatal(err)
	}
	if r := c.NAT.Rules[0]; r.DestPort != "32400" || r.Proto != "tcp" {
		t.Fatalf("dest-port/proto not applied on edit: %+v", r)
	}

	// Edit back to masquerade preserving the (still disabled) state.
	if err := c.NATRuleUpdateAt(0, "10.0.0.0/24", "", "", "", "masquerade", "eth1"); err != nil {
		t.Fatal(err)
	}
	if r := c.NAT.Rules[0]; r.Translate != "masquerade" || r.Interface != "eth1" || r.Enabled {
		t.Fatalf("masquerade edit wrong: %+v", r)
	}

	// Masquerade without an interface is rejected (shared validation with add).
	if err := c.NATRuleUpdateAt(0, "", "", "", "", "masquerade", ""); err == nil {
		t.Error("masquerade without iface should error")
	}
	// Bad port-forward target rejected.
	if err := c.NATRuleUpdateAt(0, "", "", "", "", "port-forward:not-an-ip", ""); err == nil {
		t.Error("bad port-forward target should error")
	}
	// Out-of-range index rejected.
	if err := c.NATRuleUpdateAt(9, "", "", "", "", "masquerade", "eth0"); err == nil {
		t.Error("out-of-range index should error")
	}
}

// TestForwardingEnabledDefaultsOn covers config.Config.ForwardingEnabled:
// nil (the zero value, and what every config from before this field
// existed has) means host IP forwarding is turned on at startup; only an
// explicit false opts out.
func TestForwardingEnabledDefaultsOn(t *testing.T) {
	c := natTestCfg()
	if !c.ForwardingEnabled() {
		t.Error("a fresh config (IPForwarding unset) should default to forwarding enabled")
	}
	off := false
	c.IPForwarding = &off
	if c.ForwardingEnabled() {
		t.Error("IPForwarding explicitly false should disable forwarding")
	}
	on := true
	c.IPForwarding = &on
	if !c.ForwardingEnabled() {
		t.Error("IPForwarding explicitly true should enable forwarding")
	}
}

// NAT moved from per-network to node-global in v953. A config on disk has the
// old shape; getting the hoist wrong stops translation on every LAN a node
// masquerades for.
func TestNATMigratesFromPerNetwork(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24", NAT: NAT{
			Enabled: true,
			Rules: []NATRule{
				{Source: "10.0.0.0/24", Translate: "masquerade", Interface: "eth0", Enabled: true},
				{Source: "10.0.1.0/24", Translate: "198.51.100.8", Enabled: false},
			},
		}},
		// A network whose NAT switch was off. Its rules must come across
		// disabled: the per-network gate that was holding them off has no
		// equivalent any more, so it folds into the rules themselves.
		{ID: "2222", Name: "dmz", Enabled: true, Subnet4: "10.9.0.0/24", NAT: NAT{
			Enabled: false,
			Rules:   []NATRule{{Source: "10.9.0.0/24", Translate: "masquerade", Interface: "eth1", Enabled: true}},
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v952 NAT config no longer validates: %v", err)
	}
	if len(c.NAT.Rules) != 3 {
		t.Fatalf("want all three rules hoisted, got %d: %+v", len(c.NAT.Rules), c.NAT.Rules)
	}
	// No rule carries a scope: as of v966 the selector is gone and the field
	// is cleared on load (see NATRule.Scope). What decides where a rule
	// applies is the rule, so what must survive the hoist is its interface.
	for i := range c.NAT.Rules {
		if c.NAT.Rules[i].Scope != "" {
			t.Errorf("rule %d: scope %q survived the load; it should be cleared", i, c.NAT.Rules[i].Scope)
		}
	}
	for i, want := range []string{"eth0", "", "eth1"} {
		if c.NAT.Rules[i].Interface != want {
			t.Errorf("rule %d: interface %q, want %q — the hoist lost what now decides where the rule applies", i, c.NAT.Rules[i].Interface, want)
		}
	}
	// Enabled state: rule 0 was on, rule 1 was off, rule 2 was on but its
	// network's switch was off.
	for i, want := range []bool{true, false, false} {
		if c.NAT.Rules[i].Enabled != want {
			t.Errorf("rule %d: enabled %v, want %v — the node is not translating what it translated before",
				i, c.NAT.Rules[i].Enabled, want)
		}
	}
	if !c.NAT.Enabled {
		t.Error("a node with NAT on for any network came back with NAT off")
	}
	// Cleared, so it is never written back out and cannot fight the new field
	// on the next load.
	for i := range c.Networks {
		if len(c.Networks[i].NAT.Rules) != 0 || c.Networks[i].NAT.Enabled {
			t.Errorf("network %s kept its legacy NAT and will write it back", c.Networks[i].Name)
		}
	}
}

// A config already node-global is never second-guessed.
func TestNATMigrationLeavesNewConfigsAlone(t *testing.T) {
	c := Default()
	c.NAT = NAT{Enabled: true, Rules: []NATRule{{Source: "192.168.1.0/24", Translate: "masquerade", Interface: "eth0", Enabled: true}}}
	c.Networks = []Network{{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
		NAT: NAT{Enabled: true, Rules: []NATRule{{Source: "10.0.0.0/24", Translate: "masquerade", Enabled: true}}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.NAT.Rules) != 1 || c.NAT.Rules[0].Source != "192.168.1.0/24" {
		t.Errorf("the migration overwrote a config that already had node-global rules: %+v", c.NAT.Rules)
	}
}

// The point of the whole change: a node with no mesh network at all can write
// a NAT rule. Through v952 this was impossible — the rules lived under a
// network, so a plain LAN router had nowhere to put one.
func TestNATWorksWithNoMeshNetworks(t *testing.T) {
	c := Default()
	c.Networks = nil
	if err := c.NATRuleAdd("192.168.1.0/24", "", "", "", "masquerade", "eth0"); err != nil {
		t.Fatalf("a node with no mesh networks could not add a NAT rule: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.NAT.Rules) != 1 {
		t.Fatalf("rule not stored: %+v", c.NAT.Rules)
	}
	if c.NAT.Enabled {
		t.Error("adding a rule enabled NAT; see TestNATRuleAddFull")
	}
	// There is no scope to get wrong any more, so there is no typo to refuse:
	// a second rule on a node with no networks is just another ordinary rule.
	if err := c.NATRuleAdd("10.0.0.0/24", "", "", "", "masquerade", "eth1"); err != nil {
		t.Errorf("a second rule on a network-less node was refused: %v", err)
	}
}
