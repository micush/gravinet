package config

import (
	"encoding/json"
	"testing"
)

func TestHostAddDelete(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.HostAdd("lan", "web.local", "192.168.5.5"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsAdvertise) != 1 || c.Networks[0].HostsAdvertise[0].IP != "192.168.5.5" {
		t.Fatalf("not added: %+v", c.Networks[0].HostsAdvertise)
	}
	// update in place
	if err := c.HostAdd("lan", "web.local", "192.168.5.9"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsAdvertise) != 1 || c.Networks[0].HostsAdvertise[0].IP != "192.168.5.9" {
		t.Fatalf("not updated: %+v", c.Networks[0].HostsAdvertise)
	}
	if err := c.HostAdd("lan", "bad", "not-an-ip"); err == nil {
		t.Error("invalid IP should error")
	}
	if err := c.HostDelete("lan", "web.local"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsAdvertise) != 0 {
		t.Error("not deleted")
	}
	if err := c.HostDelete("lan", "nope"); err == nil {
		t.Error("deleting missing record should error")
	}
}

func TestHostSetEnabled(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.HostAdd("lan", "web.local", "192.168.5.5"); err != nil {
		t.Fatal(err)
	}
	// New records are advertised by default (zero value of Disabled).
	if c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("new host record should default to enabled")
	}
	// Disable it.
	if err := c.HostSetEnabled("lan", "web.local", false); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("record should be disabled")
	}
	// Re-adding to change the IP must preserve the disabled state (only the
	// dedicated enable op flips it back).
	if err := c.HostAdd("lan", "web.local", "192.168.5.9"); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("re-adding to update IP must not re-enable the record")
	}
	if c.Networks[0].HostsAdvertise[0].IP != "192.168.5.9" {
		t.Fatalf("IP not updated: %+v", c.Networks[0].HostsAdvertise[0])
	}
	// Re-enable it.
	if err := c.HostSetEnabled("lan", "web.local", true); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("record should be enabled again")
	}
	// Toggling an unknown record errors.
	if err := c.HostSetEnabled("lan", "nope", false); err == nil {
		t.Error("toggling a missing record should error")
	}
}

// TestHostRecordBackwardCompat guards the chosen polarity: a config written
// before the Disabled field existed (name/ip only) must load as enabled, so
// existing advertised hosts keep being advertised after an upgrade.
func TestHostRecordBackwardCompat(t *testing.T) {
	var h HostRecord
	if err := json.Unmarshal([]byte(`{"name":"web.local","ip":"10.0.0.5"}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.Disabled {
		t.Fatal("a record with no disabled field must load as enabled")
	}
	// And an enabled record marshals without the disabled key (omitempty),
	// keeping configs clean.
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"name":"web.local","ip":"10.0.0.5"}` {
		t.Fatalf("enabled record should omit disabled key, got %s", got)
	}
}

func TestHostUpdate(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.HostAdd("lan", "web.local", "192.168.5.5"); err != nil {
		t.Fatal(err)
	}
	if err := c.HostAdd("lan", "db.local", "192.168.5.6"); err != nil {
		t.Fatal(err)
	}
	// Disable web.local so we can confirm the edit preserves state and position.
	if err := c.HostSetEnabled("lan", "web.local", false); err != nil {
		t.Fatal(err)
	}

	// Change only the IP (newName == oldName).
	if err := c.HostUpdate("lan", "web.local", "web.local", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].HostsAdvertise[0].IP != "10.0.0.9" {
		t.Fatalf("ip not updated: %+v", c.Networks[0].HostsAdvertise[0])
	}
	if !c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("editing the ip must preserve disabled state")
	}

	// Rename, preserving position (index 0) and disabled state.
	if err := c.HostUpdate("lan", "web.local", "www.local", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].HostsAdvertise[0].Name != "www.local" {
		t.Fatalf("rename did not take effect / changed position: %+v", c.Networks[0].HostsAdvertise)
	}
	if !c.Networks[0].HostsAdvertise[0].Disabled {
		t.Fatal("rename must preserve disabled state")
	}
	if len(c.Networks[0].HostsAdvertise) != 2 {
		t.Fatalf("rename should not add/drop records: %+v", c.Networks[0].HostsAdvertise)
	}

	// Renaming onto another existing record is rejected.
	if err := c.HostUpdate("lan", "www.local", "db.local", "10.0.0.9"); err == nil {
		t.Error("renaming onto an existing record should error")
	}
	// Editing an unknown record errors.
	if err := c.HostUpdate("lan", "nope", "nope2", "10.0.0.1"); err == nil {
		t.Error("updating a missing record should error")
	}
	// An invalid IP is rejected.
	if err := c.HostUpdate("lan", "www.local", "www.local", "not-an-ip"); err == nil {
		t.Error("invalid ip should error")
	}
	// Empty name is rejected.
	if err := c.HostUpdate("lan", "www.local", "", "10.0.0.9"); err == nil {
		t.Error("empty name should error")
	}
}

func TestHostRejectOps(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}

	if err := c.HostRejectAdd("lan", "bad.local"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsReject) != 1 || c.Networks[0].HostsReject[0].Name != "bad.local" {
		t.Fatalf("reject not added: %+v", c.Networks[0].HostsReject)
	}
	// New reject entries are active by default.
	if c.Networks[0].HostsReject[0].Disabled {
		t.Fatal("new reject should be enabled")
	}
	// Empty name rejected.
	if err := c.HostRejectAdd("lan", "  "); err == nil {
		t.Error("empty reject name should error")
	}
	// Disable / enable.
	if err := c.HostRejectSetEnabled("lan", "bad.local", false); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[0].HostsReject[0].Disabled {
		t.Fatal("reject should be disabled")
	}
	if err := c.HostRejectSetEnabled("lan", "BAD.LOCAL", true); err != nil { // case-insensitive
		t.Fatalf("case-insensitive toggle: %v", err)
	}
	if c.Networks[0].HostsReject[0].Disabled {
		t.Fatal("reject should be enabled again")
	}
	// Re-adding a disabled entry re-enables it (no duplicate).
	if err := c.HostRejectSetEnabled("lan", "bad.local", false); err != nil {
		t.Fatal(err)
	}
	if err := c.HostRejectAdd("lan", "bad.local"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsReject) != 1 || c.Networks[0].HostsReject[0].Disabled {
		t.Fatalf("re-add should re-enable without duplicating: %+v", c.Networks[0].HostsReject)
	}
	// Delete.
	if err := c.HostRejectDelete("lan", "bad.local"); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].HostsReject) != 0 {
		t.Fatalf("reject not deleted: %+v", c.Networks[0].HostsReject)
	}
	// Toggling/deleting a missing entry errors.
	if err := c.HostRejectSetEnabled("lan", "nope", false); err == nil {
		t.Error("toggling a missing reject should error")
	}
	if err := c.HostRejectDelete("lan", "nope"); err == nil {
		t.Error("deleting a missing reject should error")
	}
}
