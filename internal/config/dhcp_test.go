package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// A disabled row is parked, not deleted, and a relay with nothing to forward
// to is not active.
func TestDHCPRelayActive(t *testing.T) {
	for name, r := range map[string]DHCPRelayConfig{
		"no servers":   {Links: []DHCPRelayLink{{Iface: "eth1"}}},
		"no interface": {Links: []DHCPRelayLink{{Servers: []string{"10.0.0.5"}}}},
		"parked link":  {Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}, Disabled: true}}},
		"no links":     {},
	} {
		if (DHCPConfig{Mode: DHCPRelay, Relay: r}).RelayActive() {
			t.Errorf("%s: relay reported active with nothing to do", name)
		}
	}
	live := DHCPConfig{Mode: DHCPRelay, Relay: DHCPRelayConfig{
		Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}}},
	}}
	if !live.RelayActive() {
		t.Error("a configured, enabled link is not reported as active")
	}
	// The mode is the whole relay's switch, and every caller gets it for free
	// rather than each having to remember to check it first.
	off := live
	off.Mode = DHCPOff
	if len(off.EnabledLinks()) != 0 || off.RelayActive() {
		t.Error("links are in service with the relay switched off")
	}
}

// Parked links are validated too. Validating only what is running means a link
// broken during an edit made while the relay was off saves cleanly and fails
// when somebody switches it on — the moment they least want to be debugging an
// address.
func TestDHCPValidatesParkedLinksToo(t *testing.T) {
	c := DHCPConfig{Mode: DHCPOff, Relay: DHCPRelayConfig{
		Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"nope"}, Disabled: true}},
	}}
	if err := c.Validate(); err == nil {
		t.Error("a broken relay server saved cleanly because the link was parked and the relay off")
	}
}

func TestDHCPRelayValidation(t *testing.T) {
	for name, l := range map[string]DHCPRelayLink{
		"broadcast server": {Iface: "eth1", Servers: []string{"255.255.255.255"}},
		"unspecified":      {Iface: "eth1", Servers: []string{"0.0.0.0"}},
		"multicast":        {Iface: "eth1", Servers: []string{"224.0.0.1"}},
		"ipv6 server":      {Iface: "eth1", Servers: []string{"fd00::1"}},
		"hops too high":    {Iface: "eth1", MaxHops: 99},
		"hops negative":    {Iface: "eth1", MaxHops: -1},
	} {
		if err := (DHCPRelayConfig{Links: []DHCPRelayLink{l}}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (DHCPRelayConfig{Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}, MaxHops: 4}}}).Validate(); err != nil {
		t.Errorf("an ordinary relay config was rejected: %v", err)
	}
	// One socket can only be bound once, so a second row for the same link is
	// a second answer to the same question rather than a second relay.
	dup := DHCPRelayConfig{Links: []DHCPRelayLink{
		{Iface: "eth1", Servers: []string{"10.0.0.5"}},
		{Iface: "ETH1", Servers: []string{"10.0.0.6"}},
	}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "more than one relay entry") {
		t.Errorf("two relay entries on one interface accepted: %v", err)
	}
}

// The whole config's Validate has to reach the DHCP block, or none of the
// above runs on a real save.
func TestConfigValidateReachesDHCP(t *testing.T) {
	c := &Config{UDPPorts: []int{51820}}
	c.DHCP = DHCPConfig{Mode: "sideways"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "dhcp") {
		t.Errorf("Config.Validate does not check the DHCP block: %v", err)
	}
}

// --- the v988 server retirement ----------------------------------------
//
// gravinet served leases through Kea until v988. A node that was doing so has
// "server" in the mode field on disk, and ValidDHCPMode does not know that
// word any more — so the migration is what stands between this release and a
// daemon that will not start on exactly the nodes it affects most.

func TestServerModeConfigStillLoads(t *testing.T) {
	// The shape v987 wrote: a mode this release does not have, and a subnets
	// array with no field left to land in.
	raw := []byte(`{
	  "udp_ports": [51820],
	  "dhcp": {
	    "mode": "server",
	    "subnets": [{"iface":"eth1","subnet":"10.1.1.0/24","pool_start":"10.1.1.100","pool_end":"10.1.1.200"}],
	    "relay": {"links": [{"iface":"eth2","servers":["10.0.0.5"]}]}
	  }
	}`)
	c := Default()
	if err := json.Unmarshal(raw, c); err != nil {
		t.Fatalf("a v987 config no longer parses: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v987 server-mode config no longer validates, so the daemon would not start: %v", err)
	}
	// Off, not relay. The node has a relay link configured — switched away
	// from at some point — and turning it on during an upgrade would start
	// forwarding this LAN's requests to an address typed in months ago.
	if c.DHCP.Mode != DHCPOff {
		t.Errorf("server mode migrated to %q, want off", string(c.DHCP.Mode))
	}
	if c.DHCP.RelayActive() {
		t.Error("the upgrade switched the relay on by itself")
	}
	// The links themselves are kept, so an operator who does want the relay
	// has it one pill away rather than having to retype it.
	if len(c.DHCP.Relay.Links) != 1 {
		t.Errorf("the relay links were discarded along with the server: %v", c.DHCP.Relay.Links)
	}
	if !c.DHCP.RetiredServerMode() {
		t.Error("nothing records that this config was serving, so the daemon cannot say so at startup")
	}
}

// The retired mode is recognised at the file, not at the keyboard. Somebody
// asking for it now is asking for a role that no longer exists, and being told
// their value is unrecognised — when it was the documented answer one release
// ago — explains nothing about what happened to it.
func TestSelectingServerModeIsRefusedByName(t *testing.T) {
	err := ValidDHCPMode(DHCPMode("server"))
	if err == nil {
		t.Fatal("server is still accepted as a selectable mode")
	}
	if !strings.Contains(err.Error(), "no longer") {
		t.Errorf("the refusal does not say the role was removed: %v", err)
	}
	if err := ValidDHCPMode(DHCPMode("sideways")); err == nil {
		t.Error("an unknown mode was accepted")
	}
	for _, m := range []DHCPMode{DHCPOff, DHCPRelay} {
		if err := ValidDHCPMode(m); err != nil {
			t.Errorf("%q rejected: %v", string(m), err)
		}
	}
}

// The subnets are gone from the file after the first save, and the flag with
// them: it describes the file that was read, not a setting, so it must not be
// written back out or it would outlive what it describes.
func TestRetiredServerStateIsNotWrittenBack(t *testing.T) {
	c := Default()
	c.DHCP = DHCPConfig{Mode: dhcpModeRetiredServer}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, gone := range []string{"server", "subnets", "retired"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("%q survives into the written config: %s", gone, b)
		}
	}
}

// The pre-v949 relay shape — one interface list sharing one server list and
// one hop limit — has to keep parsing, and has to come back as exactly the
// relaying the node was already doing. A node upgrading has one of these on
// disk; getting it wrong stops DHCP on every LAN it relays for.
func TestDHCPRelayMigratesTheLegacyShape(t *testing.T) {
	c := Default()
	c.DHCP = DHCPConfig{Mode: DHCPRelay, Relay: DHCPRelayConfig{
		LegacyInterfaces: []string{"eth1", "eth2"},
		LegacyServers:    []string{"10.0.0.5", "10.0.0.6"},
		LegacyMaxHops:    6,
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v948 relay config no longer validates: %v", err)
	}
	links := c.DHCP.Relay.Links
	if len(links) != 2 {
		t.Fatalf("want one link per legacy interface, got %d", len(links))
	}
	for i, want := range []string{"eth1", "eth2"} {
		if links[i].Iface != want {
			t.Errorf("link %d: iface %q, want %q", i, links[i].Iface, want)
		}
		// Each link carries a copy of what used to be shared, so the node
		// relays exactly where it did before.
		if len(links[i].Servers) != 2 || links[i].MaxHops != 6 {
			t.Errorf("link %d: servers %v hops %d, want both legacy values carried",
				i, links[i].Servers, links[i].MaxHops)
		}
	}
	if !c.DHCP.RelayActive() {
		t.Error("a node that was relaying before the migration is not relaying after it")
	}
	// Cleared, so they are never written back out and cannot fight the new
	// field on the next load.
	if c.DHCP.Relay.LegacyInterfaces != nil || c.DHCP.Relay.LegacyServers != nil || c.DHCP.Relay.LegacyMaxHops != 0 {
		t.Error("the legacy relay fields survived the migration and will be written back")
	}
}

// A config already using the new shape is never second-guessed, even if the
// legacy keys are somehow also present.
func TestDHCPRelayMigrationLeavesNewConfigsAlone(t *testing.T) {
	c := Default()
	c.DHCP = DHCPConfig{Mode: DHCPRelay, Relay: DHCPRelayConfig{
		Links:            []DHCPRelayLink{{Iface: "eth9", Servers: []string{"10.9.9.9"}}},
		LegacyInterfaces: []string{"eth1"},
		LegacyServers:    []string{"10.0.0.5"},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(c.DHCP.Relay.Links) != 1 || c.DHCP.Relay.Links[0].Iface != "eth9" {
		t.Errorf("the migration overwrote a config that already had links: %v", c.DHCP.Relay.Links)
	}
}

// Whitespace-only servers are dropped rather than forwarded to, so a row typed
// with a trailing comma does not put the relay in service against nothing.
func TestDHCPRelayLinkTrimsItsServers(t *testing.T) {
	c := DHCPConfig{Mode: DHCPRelay, Relay: DHCPRelayConfig{
		Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"  ", ""}}},
	}}
	if c.RelayActive() {
		t.Error("a link whose only servers are blank is in service")
	}
}
