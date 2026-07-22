package main

import (
	"testing"

	"gravinet/internal/config"
)

func TestChooseSubnets(t *testing.T) {
	base := func(subnets ...string) *config.Config {
		c := &config.Config{}
		for _, s := range subnets {
			c.Networks = append(c.Networks, config.Network{Subnet4: s})
		}
		return c
	}
	// explicit dual-stack
	if v4, v6 := chooseSubnets(base(), []string{"corp", "subnet", "10.50.0.0/16", "subnet6", "fd00:abcd::/48"}); v4 != "10.50.0.0/16" || v6 != "fd00:abcd::/48" {
		t.Errorf("dual: got %s / %s", v4, v6)
	}
	// v4-only
	if v4, v6 := chooseSubnets(base(), []string{"corp", "subnet", "172.16.0.0/12"}); v4 != "172.16.0.0/12" || v6 != "" {
		t.Errorf("v4-only: got %s / %s", v4, v6)
	}
	// v6-only
	if v4, v6 := chooseSubnets(base(), []string{"corp", "subnet6", "fd00:6::/64"}); v4 != "" || v6 != "fd00:6::/64" {
		t.Errorf("v6-only: got %s / %s", v4, v6)
	}
	// flag form
	if v4, v6 := chooseSubnets(base(), []string{"corp", "-subnet", "10.99.0.0/16", "--subnet6=fd00:99::/64"}); v4 != "10.99.0.0/16" || v6 != "fd00:99::/64" {
		t.Errorf("flag form: got %s / %s", v4, v6)
	}
	// none -> auto dual-stack, non-overlapping with the existing 10.42
	if v4, v6 := chooseSubnets(base("10.42.0.0/16"), []string{"corp"}); v4 != "10.43.0.0/16" || v6 == "" {
		t.Errorf("auto: got %s / %s", v4, v6)
	}
}

func TestNextFreeSubnets(t *testing.T) {
	cfgWith := func(subnets ...string) *config.Config {
		c := &config.Config{}
		for _, s := range subnets {
			c.Networks = append(c.Networks, config.Network{Subnet4: s})
		}
		return c
	}

	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"empty", cfgWith(), "10.42.0.0/16"},
		{"after default", cfgWith("10.42.0.0/16"), "10.43.0.0/16"},
		{"two in a row", cfgWith("10.42.0.0/16", "10.43.0.0/16"), "10.44.0.0/16"},
		{"fills a gap", cfgWith("10.42.0.0/16", "10.44.0.0/16"), "10.43.0.0/16"},
		{"ignores non-10 subnets", cfgWith("192.168.50.0/24"), "10.42.0.0/16"},
	}
	for _, tc := range cases {
		v4, v6 := nextFreeSubnets(tc.cfg)
		if v4 != tc.want {
			t.Errorf("%s: got v4 %s, want %s", tc.name, v4, tc.want)
		}
		if v6 == "" {
			t.Errorf("%s: empty v6", tc.name)
		}
	}
}
