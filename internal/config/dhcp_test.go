package config

import (
	"strings"
	"testing"
)

func goodSubnet() DHCPSubnet {
	return DHCPSubnet{
		Iface: "eth1", Subnet: "10.1.1.0/24",
		PoolStart: "10.1.1.100", PoolEnd: "10.1.1.200", Router: "10.1.1.1",
		DNS: []string{"10.1.1.1"},
	}
}

func TestDHCPSubnetAcceptsAnOrdinaryScope(t *testing.T) {
	if err := goodSubnet().Validate(); err != nil {
		t.Fatalf("a plain LAN scope was rejected: %v", err)
	}
}

// The errors worth catching on save are the ones that otherwise produce a
// server which starts, runs, logs nothing unusual, and hands out addresses
// that do not work.
func TestDHCPSubnetRejectsUnservableScopes(t *testing.T) {
	cases := map[string]func(*DHCPSubnet){
		// A pool outside its own subnet leases addresses nothing on the link
		// can use.
		"pool outside subnet": func(s *DHCPSubnet) { s.PoolStart, s.PoolEnd = "10.2.2.100", "10.2.2.200" },
		"pool end outside":    func(s *DHCPSubnet) { s.PoolEnd = "10.2.2.200" },
		"pool reversed":       func(s *DHCPSubnet) { s.PoolStart, s.PoolEnd = "10.1.1.200", "10.1.1.100" },
		// A gateway inside the pool gets leased to a client, and then two
		// hosts answer for the same address.
		"router inside pool":   func(s *DHCPSubnet) { s.Router = "10.1.1.150" },
		"router off subnet":    func(s *DHCPSubnet) { s.Router = "10.9.9.1" },
		"subnet has host bits": func(s *DHCPSubnet) { s.Subnet = "10.1.1.5/24" },
		"no interface":         func(s *DHCPSubnet) { s.Iface = "" },
		"bad subnet":           func(s *DHCPSubnet) { s.Subnet = "not-a-cidr" },
		"bad pool address":     func(s *DHCPSubnet) { s.PoolStart = "hello" },
		"ipv4 dns required":    func(s *DHCPSubnet) { s.DNS = []string{"fd00::1"} },
		"negative lease":       func(s *DHCPSubnet) { s.LeaseSeconds = -1 },
		// DHCPv6 is not this feature. Accepting a v6 scope here would render
		// it into a v4 server that silently ignores it.
		"ipv6 subnet": func(s *DHCPSubnet) {
			s.Subnet, s.PoolStart, s.PoolEnd, s.Router = "fd00::/64", "fd00::100", "fd00::200", ""
		},
	}
	for name, mangle := range cases {
		s := goodSubnet()
		mangle(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The mutual exclusion, which is the whole reason Mode is one field rather
// than two enable flags. Every caller gets it from these two accessors instead
// of having to remember to check the mode first.
func TestDHCPModeIsExclusive(t *testing.T) {
	full := DHCPConfig{
		Subnets: []DHCPSubnet{goodSubnet()},
		Relay: DHCPRelayConfig{
			Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}}},
		},
	}

	off := full
	off.Mode = DHCPOff
	if len(off.EnabledSubnets()) != 0 || off.RelayActive() {
		t.Error("mode off must serve nothing and relay nothing")
	}

	srv := full
	srv.Mode = DHCPServer
	if len(srv.EnabledSubnets()) != 1 {
		t.Error("server mode should serve its subnets")
	}
	if srv.RelayActive() {
		t.Error("server mode must not also relay — that is the case this model exists to prevent")
	}

	rly := full
	rly.Mode = DHCPRelay
	if !rly.RelayActive() {
		t.Error("relay mode should relay")
	}
	if len(rly.EnabledSubnets()) != 0 {
		t.Error("relay mode must not also serve")
	}

	// Switching away and back must not have discarded the other half's
	// configuration: an operator who relays for an afternoon should not have
	// to retype their pools.
	if len(rly.Subnets) != 1 || len(srv.Relay.Links) != 1 {
		t.Error("switching mode discarded the inactive half's configuration")
	}
}

// A disabled row is parked, not deleted, and a relay with nothing to forward
// to is not active.
func TestDHCPEnabledSubnetsAndRelayActive(t *testing.T) {
	s := goodSubnet()
	s.Disabled = true
	c := DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{s}}
	if len(c.EnabledSubnets()) != 0 {
		t.Error("a disabled subnet is still being served")
	}
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
}

// Both halves are validated whichever mode is selected. Validating only the
// active half means a subnet broken during an edit made in relay mode saves
// cleanly and fails when somebody switches back — which is the moment they
// least want to be debugging a pool boundary.
func TestDHCPValidatesTheInactiveHalfToo(t *testing.T) {
	bad := goodSubnet()
	bad.PoolEnd = "10.2.2.200"
	if err := (DHCPConfig{Mode: DHCPRelay, Subnets: []DHCPSubnet{bad}}).Validate(); err == nil {
		t.Error("a broken subnet saved cleanly because the node was in relay mode")
	}
	if err := (DHCPConfig{Mode: DHCPServer, Relay: DHCPRelayConfig{Links: []DHCPRelayLink{{Iface: "eth1", Servers: []string{"nope"}}}}}).Validate(); err == nil {
		t.Error("a broken relay server saved cleanly because the node was in server mode")
	}
	// Two *attached* pools on one interface is not a second scope, it is two
	// answers to the same question. Narrowed to attached subnets in v969 —
	// the relayed case is its own test below, and the rule it obeys instead
	// is uniqueness of the relay address.
	dup := DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{goodSubnet(), goodSubnet()}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "more than one directly attached subnet") {
		t.Errorf("two attached subnets on one interface accepted: %v", err)
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

// --- relayed subnets (v969) --------------------------------------------
//
// Serving a network this node is not attached to. The rules below are the
// ones that make it possible at all: an interface may carry any number of
// relayed subnets, because what tells them apart is the giaddr rather than
// the link.

func relayedSubnet(prefix, giaddr string) DHCPSubnet {
	return DHCPSubnet{
		Iface: "eth0", Subnet: prefix + ".0/24", Relays: []string{giaddr},
		PoolStart: prefix + ".100", PoolEnd: prefix + ".200", Router: prefix + ".1",
	}
}

// The headline: one uplink, several branch LANs behind relays. Before v969
// this was refused on the second row, which capped a node at a single remote
// network for no reason that applied to it.
func TestDHCPManyRelayedSubnetsShareOneInterface(t *testing.T) {
	c := DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{
		relayedSubnet("10.9.1", "10.9.1.1"),
		relayedSubnet("10.9.2", "10.9.2.1"),
		relayedSubnet("10.9.3", "10.9.3.1"),
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("three relayed subnets on one interface were refused: %v", err)
	}
	if n := len(c.EnabledSubnets()); n != 3 {
		t.Errorf("EnabledSubnets = %d, want all three in service", n)
	}
}

// A node with a LAN of its own and remote ones behind the same interface is
// an ordinary configuration and needs no extra setting to express.
func TestDHCPAttachedAndRelayedSubnetsCoexistOnOneInterface(t *testing.T) {
	local := goodSubnet()
	local.Iface = "eth0"
	remote := relayedSubnet("10.9.1", "10.9.1.1")
	if err := (DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{local, remote}}).Validate(); err != nil {
		t.Fatalf("an attached subnet and a relayed one on one interface were refused: %v", err)
	}
	// The attached rule still bites for a genuine second attached subnet on
	// that link, which the relaxation must not have taken with it.
	second := goodSubnet()
	second.Iface, second.Subnet = "eth0", "10.4.4.0/24"
	second.PoolStart, second.PoolEnd, second.Router = "10.4.4.100", "10.4.4.200", "10.4.4.1"
	second.DNS = nil
	if err := (DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{local, second}}).Validate(); err == nil {
		t.Error("relaxing the rule for relayed subnets also relaxed it for attached ones")
	}
}

// The replacement rule. Kea selects a relayed scope by matching giaddr, so an
// address on two subnets is two answers to one question — and a quiet one,
// since both scopes are valid and one LAN simply gets the other's addresses.
func TestDHCPRelayAddressIsUniqueAcrossSubnets(t *testing.T) {
	a := relayedSubnet("10.9.1", "192.168.50.1")
	b := relayedSubnet("10.9.2", "192.168.50.1")
	err := (DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{a, b}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "listed on both") {
		t.Fatalf("one relay address on two subnets accepted: %v", err)
	}
	// Both prefixes named, so the operator does not have to go and find
	// which other row they collided with.
	for _, want := range []string{"10.9.1.0/24", "10.9.2.0/24", "192.168.50.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// Two relays on one segment — a VRRP pair being the ordinary case — forward
// under their own addresses, and both have to land in the same scope or a
// client's answer depends on which router was master when it asked.
func TestDHCPSubnetTakesSeveralRelayAddresses(t *testing.T) {
	s := relayedSubnet("10.9.1", "10.9.1.2")
	s.Relays = []string{"10.9.1.2", "10.9.1.3"}
	if err := s.Validate(); err != nil {
		t.Fatalf("a subnet behind a redundant relay pair was refused: %v", err)
	}
	if got := len(s.RelayAddrs()); got != 2 {
		t.Errorf("RelayAddrs = %d, want 2", got)
	}
}

func TestDHCPRelayAddressValidation(t *testing.T) {
	for name, addr := range map[string]string{
		"broadcast":   "255.255.255.255",
		"unspecified": "0.0.0.0",
		"multicast":   "224.0.0.1",
		"not an ip":   "the-router",
		"v6":          "2001:db8::1",
	} {
		s := relayedSubnet("10.9.1", addr)
		if err := s.Validate(); err == nil {
			t.Errorf("%s accepted as a relay address: a scope selected by it can never be reached", name)
		}
	}
}

// A relay agent usually forwards under its own address on the client link,
// but it is only usually: sourcing from a loopback or management address is
// permitted by RFC 1542 and happens in the field. Refusing it would refuse a
// working network, so the giaddr is deliberately not checked against Subnet.
func TestDHCPRelayAddressNeedNotBeInsideTheSubnetItSelects(t *testing.T) {
	s := relayedSubnet("10.9.1", "172.16.0.9")
	if err := s.Validate(); err != nil {
		t.Fatalf("a relay sourcing from off-segment was refused: %v", err)
	}
}

// Relayed() is what the renderer, the preflight and both duplicate rules
// branch on, so whitespace deciding it differently in one of them would be a
// scope rendered one way and judged another.
func TestDHCPRelayedIgnoresBlankEntries(t *testing.T) {
	s := goodSubnet()
	s.Relays = []string{"", "   "}
	if s.Relayed() {
		t.Error("a row with nothing but blanks in the relay column counts as relayed")
	}
	s.Relays = []string{" 10.9.1.1 "}
	if !s.Relayed() || s.RelayAddrs()[0] != "10.9.1.1" {
		t.Errorf("a padded relay address did not survive trimming: %v", s.RelayAddrs())
	}
}

// The interface is still required on a relayed subnet, and for a reason that
// is easy to talk oneself out of: the subnet is not on it, but Kea only
// listens where it is told, so it is what gets the forwarded unicast received.
func TestDHCPRelayedSubnetStillNeedsAnInterface(t *testing.T) {
	s := relayedSubnet("10.9.1", "10.9.1.1")
	s.Iface = ""
	if err := s.Validate(); err == nil {
		t.Error("a relayed subnet saved with no interface, so nothing would listen for it")
	}
}
