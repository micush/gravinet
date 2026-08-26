package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// Whether a NAT rule also applies to a network's overlay traffic is derived
// from the rule as of v966, not chosen beside it. These pin the derivation
// down, and in particular pin down that the answer still comes out the same
// for the rules operators actually had under the old NATRule.Scope selector.

func TestNATRuleAppliesToOverlayDerivation(t *testing.T) {
	cases := []struct {
		name  string
		rule  config.NATRule
		iface string
		want  bool
	}{{
		name:  "physical interface is about that interface, not the overlay",
		rule:  config.NATRule{Source: "10.0.0.0/8", Translate: "masquerade", Interface: "eth0"},
		iface: "mesh0",
		want:  false,
	}, {
		name:  "this network's own device",
		rule:  config.NATRule{Source: "10.0.0.0/8", Translate: "masquerade", Interface: "mesh0"},
		iface: "mesh0",
		want:  true,
	}, {
		name:  "another network's device",
		rule:  config.NATRule{Source: "10.0.0.0/8", Translate: "masquerade", Interface: "mesh1"},
		iface: "mesh0",
		want:  false,
	}, {
		// A static SNAT or port-forward names no interface, so nothing
		// constrains it but its own prefixes — the case where the address
		// predicates really are the whole definition.
		name:  "no interface: prefixes are the whole definition",
		rule:  config.NATRule{Source: "192.168.1.0/24", Dest: "10.2.2.0/24", Translate: "198.51.100.7"},
		iface: "mesh0",
		want:  true,
	}, {
		name:  "port-forward with no interface",
		rule:  config.NATRule{Dest: "10.255.255.203", Translate: "port-forward:10.1.1.50"},
		iface: "mesh0",
		want:  true,
	}, {
		// Interface names are matched the way every other interface
		// comparison in the config layer does it.
		name:  "case-insensitive device match",
		rule:  config.NATRule{Translate: "masquerade", Interface: "MESH0"},
		iface: "mesh0",
		want:  true,
	}, {
		// A network with no resolved device name must not swallow every
		// interface-bound rule by matching blank against blank.
		name:  "blank network device does not match a named interface",
		rule:  config.NATRule{Translate: "masquerade", Interface: "eth0"},
		iface: "",
		want:  false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := natRuleAppliesToOverlay(tc.rule, tc.iface); got != tc.want {
				t.Fatalf("natRuleAppliesToOverlay(iface=%q) = %v, want %v", tc.iface, got, tc.want)
			}
		})
	}
}

// TestInternetSharingRuleStaysKernelOnly is the shape the reporting operator
// actually had: masquerade everything in 10/8 that is *not* headed into 10/8,
// out eth0. It carried scope "mesh", which did nothing — the negated dest
// meant it could never match overlay↔overlay traffic either way. Deriving the
// answer has to reach the same place, or a working internet-sharing setup
// changes behavior on upgrade.
func TestInternetSharingRuleStaysKernelOnly(t *testing.T) {
	n := config.Network{ID: "38be68fae7802f62", Name: "mesh", Enabled: true}
	nat := config.NAT{
		Enabled: true,
		Rules: []config.NATRule{{
			Source:     "10.0.0.0/8",
			Dest:       "10.0.0.0/8",
			DestNegate: true,
			Translate:  "masquerade",
			Interface:  "eth0",
			Enabled:    true,
		}},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, "mesh0", nil, 0, nil, config.BGPConfig{}, nat, config.QoS{}, config.Throttle{}, config.Firewall{}, nil)

	if len(spec.NAT) != 0 {
		t.Fatalf("an eth0-bound masquerade reached the overlay table: %+v", spec.NAT)
	}
	// It must still be a kernel rule — dropping it from the overlay must not
	// drop it from the node.
	cfg := config.Default()
	cfg.NAT = nat
	if got := kernelNATRules(cfg); len(got) != 1 {
		t.Fatalf("expected the rule to still be programmed into the kernel, got %d: %+v", len(got), got)
	}
}

// TestOverlaySNATReachesOverlayWithoutAScope is the case the selector existed
// to serve: two sites with overlapping LANs, SNAT applied as traffic crosses
// the mesh. It names no interface, because its translate is a literal address,
// so under the old scheme an operator had to remember to pick the network by
// hand or the rule silently did nothing. It now arrives on its own.
func TestOverlaySNATReachesOverlayWithoutAScope(t *testing.T) {
	n := config.Network{ID: "1111", Name: "mesh", Enabled: true}
	nat := config.NAT{
		Enabled: true,
		Rules: []config.NATRule{{
			Source:    "192.168.1.0/24",
			Dest:      "10.2.2.0/24",
			Translate: "198.51.100.7",
			Enabled:   true,
		}},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, "mesh0", nil, 0, nil, config.BGPConfig{}, nat, config.QoS{}, config.Throttle{}, config.Firewall{}, nil)

	if len(spec.NAT) != 1 {
		t.Fatalf("an overlay SNAT rule did not reach the overlay table: %+v", spec.NAT)
	}
	if spec.NAT[0].Translate != "198.51.100.7" {
		t.Fatalf("wrong rule reached the overlay: %+v", spec.NAT[0])
	}
}

// TestStaleScopeInConfigIsIgnored: a config written before v966 still parses,
// and its scope no longer steers anything. The rule below names eth0 while
// claiming scope "mesh" — the exact contradiction the old two-field scheme
// allowed. The interface wins, because it is the one the kernel enforces.
func TestStaleScopeInConfigIsIgnored(t *testing.T) {
	cfg := config.Default()
	cfg.Networks = []config.Network{{ID: "1111", Name: "mesh", Enabled: true, Subnet4: "10.255.255.0/24"}}
	cfg.NAT = config.NAT{
		Enabled: true,
		Rules: []config.NATRule{
			{Source: "10.0.0.0/8", Translate: "masquerade", Interface: "eth0", Scope: "mesh", Enabled: true},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a pre-v966 config with a scope no longer validates: %v", err)
	}
	if got := cfg.NAT.Rules[0].Scope; got != "" {
		t.Errorf("scope %q survived load; it should be cleared so nothing writes it back out", got)
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, cfg.Networks[0], "mesh0", nil, 0, nil, config.BGPConfig{}, cfg.NAT, config.QoS{}, config.Throttle{}, config.Firewall{}, nil)
	if len(spec.NAT) != 0 {
		t.Fatalf("a stale scope steered an eth0-bound rule into the overlay: %+v", spec.NAT)
	}
}
