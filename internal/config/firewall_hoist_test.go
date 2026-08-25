package config

import "testing"

// The firewall moved from per-network to node-global in v957, and rules gained
// stable ids that the engine adopts rather than minting.
func TestFirewallMigratesFromPerNetwork(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24", Firewall: Firewall{
			Enabled: true,
			Rules: []FirewallRule{
				{Action: "deny", Proto: "tcp", DstPortMin: 22, DstPortMax: 22},
				{Action: "allow", Proto: "udp", DstPortMin: 53, DstPortMax: 53},
			},
		}},
		// Firewall switched off: its rules come across disabled, the same fold
		// NAT and QoS got — the per-network gate has no equivalent now.
		{ID: "2222", Name: "dmz", Enabled: true, Subnet4: "10.9.0.0/24", Firewall: Firewall{
			Enabled: false,
			Rules:   []FirewallRule{{Action: "deny", Proto: "tcp", DstPortMin: 445, DstPortMax: 445}},
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v956 firewall config no longer validates: %v", err)
	}
	if len(c.Firewall.Rules) != 3 {
		t.Fatalf("want all three rules hoisted, got %d", len(c.Firewall.Rules))
	}
	// Order within a network is preserved, which is what matters: first match
	// wins, and a rule only ever competes with the rules in scope alongside it.
	for i, want := range []string{"lan", "lan", "dmz"} {
		if c.Firewall.Rules[i].Scope != want {
			t.Errorf("rule %d: scope %q, want %q", i, c.Firewall.Rules[i].Scope, want)
		}
	}
	if c.Firewall.Rules[0].DstPortMin != 22 || c.Firewall.Rules[1].DstPortMin != 53 {
		t.Error("rule order within a network was not preserved")
	}
	if !c.Firewall.Rules[2].Disabled {
		t.Error("a rule from a firewall-disabled network came across enabled")
	}
	// Every rule gets a unique, non-zero id.
	seen := map[uint64]bool{}
	for i, r := range c.Firewall.Rules {
		if r.ID == 0 {
			t.Errorf("rule %d has no id; the engine would mint one and it would not survive a reload", i)
		}
		if seen[r.ID] {
			t.Errorf("rule %d reuses id %d", i, r.ID)
		}
		seen[r.ID] = true
	}
	for i := range c.Networks {
		if len(c.Networks[i].Firewall.Rules) != 0 {
			t.Errorf("network %s kept its legacy rules and will write them back", c.Networks[i].Name)
		}
	}
}

// The point of the change: a node with no mesh network can write a rule.
func TestFirewallWorksWithNoMeshNetworks(t *testing.T) {
	c := Default()
	c.Networks = nil
	if err := c.FirewallRuleAdd(FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 22, DstPortMax: 22}, -1); err != nil {
		t.Fatalf("a node with no mesh networks could not add a firewall rule: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.Firewall.Rules) != 1 || c.Firewall.Rules[0].ID == 0 {
		t.Fatalf("rule not stored with an id: %+v", c.Firewall.Rules)
	}
	if c.Firewall.Rules[0].Scope != "" {
		t.Errorf("scope should default to every network, got %q", c.Firewall.Rules[0].Scope)
	}
	if err := c.FirewallRuleAdd(FirewallRule{Action: "deny", Scope: "nope"}, -1); err == nil {
		t.Error("a scope naming no mesh network was accepted")
	}
}

// Ids are never reused, even one freed by a delete: a returning id would bind
// stale hit counters and stale UI selections to a different rule.
func TestFirewallIDsAreNeverReused(t *testing.T) {
	c := Default()
	c.Networks = nil
	for i := 0; i < 3; i++ {
		if err := c.FirewallRuleAdd(FirewallRule{Action: "allow"}, -1); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	last := c.Firewall.Rules[2].ID
	if err := c.FirewallRuleDelete([]uint64{last}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.FirewallRuleAdd(FirewallRule{Action: "deny"}, -1); err != nil {
		t.Fatalf("add after delete: %v", err)
	}
	if got := c.Firewall.Rules[len(c.Firewall.Rules)-1].ID; got == last {
		t.Errorf("id %d was handed to a new rule after the old one was deleted", got)
	}
}

// Scope decides which networks enforce a rule; blank means every one, so a
// rule written before any network exists starts enforcing once one does.
func TestFirewallRulesForScoping(t *testing.T) {
	c := Default()
	c.Networks = []Network{
		{ID: "1111", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		{ID: "2222", Name: "dmz", Enabled: true, Subnet4: "10.9.0.0/24"},
	}
	c.Firewall.Rules = []FirewallRule{
		{ID: 1, Action: "deny", Scope: "lan"},
		{ID: 2, Action: "allow"}, // every network
		{ID: 3, Action: "deny", Scope: "dmz"},
	}
	lan := c.FirewallRulesFor(c.Networks[0])
	if len(lan) != 2 || lan[0].ID != 1 || lan[1].ID != 2 {
		t.Fatalf("lan got %+v, want rules 1 and 2 in order", lan)
	}
	dmz := c.FirewallRulesFor(c.Networks[1])
	if len(dmz) != 2 || dmz[0].ID != 2 || dmz[1].ID != 3 {
		t.Fatalf("dmz got %+v, want rules 2 and 3 in order", dmz)
	}
}

// Moving a rule changes which one matches first, so it is a change in meaning.
func TestFirewallRuleMoveAndUpdateKeepIdentity(t *testing.T) {
	c := Default()
	c.Networks = nil
	for _, a := range []string{"allow", "deny", "allow"} {
		if err := c.FirewallRuleAdd(FirewallRule{Action: a}, -1); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	third := c.Firewall.Rules[2].ID
	if err := c.FirewallRuleMove(third, 0); err != nil {
		t.Fatalf("move: %v", err)
	}
	if c.Firewall.Rules[0].ID != third || len(c.Firewall.Rules) != 3 {
		t.Fatalf("move did not put the rule first without duplicating it: %+v", c.Firewall.Rules)
	}
	// An update keeps the rule's id and position — it used to be a delete plus
	// an add, which lost both along with the hit counters.
	if err := c.FirewallRuleUpdate(third, FirewallRule{Action: "deny", Notes: "edited"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.Firewall.Rules[0].ID != third || c.Firewall.Rules[0].Notes != "edited" {
		t.Fatalf("update lost the rule's identity or position: %+v", c.Firewall.Rules[0])
	}
}
