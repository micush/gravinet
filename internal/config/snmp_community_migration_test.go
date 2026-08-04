package config

import "testing"

func snmpTestCfg() *Config {
	return &Config{UDPPorts: []int{65432}, EnableIPv4: true}
}

// TestSNMPCommunityMigration checks that a config file from before SNMP
// grew a Communities list (SNMPConfig.Community, singular) still parses
// and its community string isn't silently dropped: Validate must migrate
// it into a one-entry Communities list and clear the legacy field so it's
// never written back out.
func TestSNMPCommunityMigration(t *testing.T) {
	c := snmpTestCfg()
	c.SNMP.Enabled = true
	c.SNMP.Community = "public"
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.SNMP.Communities) != 1 || c.SNMP.Communities[0].Community != "public" {
		t.Fatalf("Communities = %+v, want a single migrated \"public\" entry", c.SNMP.Communities)
	}
	if c.SNMP.Communities[0].Disabled {
		t.Error("a migrated legacy community should not come in disabled")
	}
	if c.SNMP.Community != "" {
		t.Errorf("legacy Community field = %q, want cleared after migration", c.SNMP.Community)
	}
}

// TestSNMPCommunityMigrationSkippedWhenAlreadyPresent checks the migration
// never overrides or duplicates onto an already-populated Communities list
// — e.g. a config saved by a build that already has the list, where the
// legacy field is simply absent/empty and there's nothing to migrate.
func TestSNMPCommunityMigrationSkippedWhenAlreadyPresent(t *testing.T) {
	c := snmpTestCfg()
	c.SNMP.Enabled = true
	c.SNMP.Communities = []SNMPCommunity{{Community: "monitoring"}}
	c.SNMP.Community = "" // nothing legacy to migrate
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.SNMP.Communities) != 1 || c.SNMP.Communities[0].Community != "monitoring" {
		t.Fatalf("Communities = %+v, want unchanged", c.SNMP.Communities)
	}
}

// TestSNMPCommunityMigrationRunsEvenWhenDisabled checks Validate's
// migration runs regardless of whether SNMP itself is enabled — a
// disabled agent's configured community shouldn't be lost either, so
// re-enabling it later doesn't come back empty.
func TestSNMPCommunityMigrationRunsEvenWhenDisabled(t *testing.T) {
	c := snmpTestCfg()
	c.SNMP.Enabled = false
	c.SNMP.Community = "public"
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.SNMP.Communities) != 1 || c.SNMP.Communities[0].Community != "public" {
		t.Fatalf("Communities = %+v, want migrated even while SNMP is disabled", c.SNMP.Communities)
	}
}
