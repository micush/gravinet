package config

import (
	"strings"
	"testing"
)

func preferTestConfig(t *testing.T) *Config {
	t.Helper()
	c := Default()
	n := NewNetworkDefaults()
	n.Name = "testnet"
	n.ID = "testnet"
	n.Subnet4 = "10.77.0.0/24"
	n.Routes = []Route{{CIDR: "0.0.0.0/0", Enabled: true}}
	c.Networks = append(c.Networks, n)
	return c
}

func TestRoutePreferRoundTrip(t *testing.T) {
	c := preferTestConfig(t)
	net := "testnet"
	if err := c.RoutePrefer(net, "0.0.0.0/0", []string{"nodeC", "nodeB"}); err != nil {
		t.Fatal(err)
	}
	got := c.Networks[len(c.Networks)-1].RoutePrefer
	if len(got) != 1 || got[0].CIDR != "0.0.0.0/0" {
		t.Fatalf("entry = %+v", got)
	}
	if strings.Join(got[0].Origins, ",") != "nodeC,nodeB" {
		t.Fatalf("origins = %v, want [nodeC nodeB] in that order", got[0].Origins)
	}
}

// Setting a preference replaces rather than merges: the list is positional, so
// appending would silently change what everything after it ranks against.
func TestRoutePreferReplacesRatherThanMerges(t *testing.T) {
	c := preferTestConfig(t)
	net := "testnet"
	_ = c.RoutePrefer(net, "0.0.0.0/0", []string{"nodeA", "nodeB"})
	if err := c.RoutePrefer(net, "0.0.0.0/0", []string{"nodeC"}); err != nil {
		t.Fatal(err)
	}
	got := c.Networks[len(c.Networks)-1].RoutePrefer
	if len(got) != 1 || len(got[0].Origins) != 1 || got[0].Origins[0] != "nodeC" {
		t.Fatalf("origins = %v, want exactly [nodeC]", got[0].Origins)
	}
}

// A duplicate is unreachable at its second position and shifts every rank after
// it, so it is always a mistake.
func TestRoutePreferRejectsDuplicateOrigin(t *testing.T) {
	c := preferTestConfig(t)
	if err := c.RoutePrefer(c.Networks[len(c.Networks)-1].Name, "0.0.0.0/0", []string{"nodeA", "nodeA"}); err == nil {
		t.Fatal("duplicate origin accepted")
	}
}

func TestRoutePreferEmptyListClears(t *testing.T) {
	c := preferTestConfig(t)
	net := "testnet"
	_ = c.RoutePrefer(net, "0.0.0.0/0", []string{"nodeA"})
	if err := c.RoutePrefer(net, "0.0.0.0/0", nil); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[len(c.Networks)-1].RoutePrefer) != 0 {
		t.Fatalf("entry survived an empty list: %+v", c.Networks[len(c.Networks)-1].RoutePrefer)
	}
}

// A disabled entry keeps its order but stops applying, matching the convention
// used by reject entries and firewall rules.
func TestRoutePreferSetEnabled(t *testing.T) {
	c := preferTestConfig(t)
	net := "testnet"
	_ = c.RoutePrefer(net, "0.0.0.0/0", []string{"nodeA"})
	if err := c.RoutePreferSetEnabled(net, "0.0.0.0/0", false); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[len(c.Networks)-1].RoutePrefer[0].Disabled {
		t.Fatal("entry not disabled")
	}
	if len(c.Networks[len(c.Networks)-1].RoutePrefer[0].Origins) != 1 {
		t.Fatal("disabling lost the order; re-enabling must restore it intact")
	}
	if err := c.RoutePreferSetEnabled(net, "10.0.0.0/8", true); err == nil {
		t.Fatal("enabling a nonexistent entry silently succeeded")
	}
}

func TestRoutePreferValidation(t *testing.T) {
	c := preferTestConfig(t)
	c.Networks[len(c.Networks)-1].RoutePrefer = []PreferRoute{{CIDR: "not-a-cidr", Origins: []string{"n"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("bad cidr passed validation")
	}
	c.Networks[len(c.Networks)-1].RoutePrefer = []PreferRoute{{CIDR: "0.0.0.0/0"}}
	if err := c.Validate(); err == nil {
		t.Fatal("entry with no origins passed validation")
	}
	c.Networks[len(c.Networks)-1].RoutePrefer = []PreferRoute{{CIDR: "0.0.0.0/0", Origins: []string{"a", "a"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate origin passed validation")
	}
	c.Networks[len(c.Networks)-1].RoutePrefer = []PreferRoute{{CIDR: "0.0.0.0/0", Origins: []string{"a", "b"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
}
