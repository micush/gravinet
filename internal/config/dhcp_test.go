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
			Interfaces: []string{"eth1"}, Servers: []string{"10.0.0.5"},
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
	if len(rly.Subnets) != 1 || len(srv.Relay.Servers) != 1 {
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
		"no servers":    {Interfaces: []string{"eth1"}},
		"no interfaces": {Servers: []string{"10.0.0.5"}},
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
	if err := (DHCPConfig{Mode: DHCPServer, Relay: DHCPRelayConfig{Servers: []string{"nope"}}}).Validate(); err == nil {
		t.Error("a broken relay server saved cleanly because the node was in server mode")
	}
	// Two pools on one interface is not a second scope, it is two answers to
	// the same question.
	dup := DHCPConfig{Mode: DHCPServer, Subnets: []DHCPSubnet{goodSubnet(), goodSubnet()}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "more than one subnet") {
		t.Errorf("two subnets on one interface accepted: %v", err)
	}
}

func TestDHCPRelayValidation(t *testing.T) {
	for name, r := range map[string]DHCPRelayConfig{
		"broadcast server": {Servers: []string{"255.255.255.255"}},
		"unspecified":      {Servers: []string{"0.0.0.0"}},
		"multicast":        {Servers: []string{"224.0.0.1"}},
		"ipv6 server":      {Servers: []string{"fd00::1"}},
		"hops too high":    {MaxHops: 99},
		"hops negative":    {MaxHops: -1},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (DHCPRelayConfig{Servers: []string{"10.0.0.5"}, MaxHops: 4}).Validate(); err != nil {
		t.Errorf("an ordinary relay config was rejected: %v", err)
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
