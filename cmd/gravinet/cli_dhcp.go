package main

import (
	"fmt"

	"gravinet/internal/config"
)

// cmdSystemDHCP is the CLI leaf for System > DHCP. It exists for nav parity —
// every web-admin page has a CLI counterpart, and a page reachable only in a
// browser is a page an operator cannot script or inspect over ssh.
//
// Read and mode-switch only, deliberately, the same split cmdIPv6RA draws.
// Adding a subnet needs an interface, a CIDR, both pool boundaries and a
// gateway that has to sit inside one and outside the other; reproducing that
// as flags would be a second, weaker implementation of a form the editor
// already validates. What is worth having on a terminal at 3am is seeing what
// this node is doing and being able to stop it.
func cmdSystemDHCP(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub := expandVerb(args[0], v("list"), v("mode"))
	cfg, path, rest := openCfg(args[1:])
	d := &cfg.DHCP

	switch sub {
	case "list":
		mode := string(d.Mode)
		if mode == "" {
			mode = "off"
		}
		fmt.Printf("dhcp role: %s\n", mode)
		fmt.Println("  server subnets:")
		if len(d.Subnets) == 0 {
			fmt.Println("    (none)")
		}
		for _, s := range d.Subnets {
			fmt.Printf("    %-10s %-8s %-18s pool=%s-%s router=%s dns=%s lease=%s\n",
				s.Iface, onOff(!s.Disabled), s.Subnet, s.PoolStart, s.PoolEnd,
				orDash(s.Router), orDash(joinComma(s.DNS)), orDash(leaseLabel(s.LeaseSeconds)))
		}
		fmt.Println("  relay:")
		if len(d.Relay.Interfaces) == 0 && len(d.Relay.Servers) == 0 {
			fmt.Println("    (not configured)")
		} else {
			fmt.Printf("    interfaces=%s servers=%s max_hops=%d\n",
				orDash(joinComma(d.Relay.Interfaces)), orDash(joinComma(d.Relay.Servers)), d.Relay.MaxHops)
		}
		// Both halves are listed whichever is running, because both are
		// stored whichever is running — switching to relay for an afternoon
		// does not discard the pools. Which one is live is the role above.

	case "mode":
		if len(rest) == 0 {
			fatal("usage: gravinet system dhcp mode <off|server|relay>")
		}
		m := config.DHCPMode(rest[0])
		if rest[0] == "off" {
			m = config.DHCPOff
		}
		if err := config.ValidDHCPMode(m); err != nil {
			fatal("%v", err)
		}
		d.Mode = m
		if err := cfg.Validate(); err != nil {
			fatal("invalid config after change: %v", err)
		}
		if err := cfg.SaveTo(path); err != nil {
			fatal("save config: %v", err)
		}
		fmt.Printf("dhcp role set to %s\n", rest[0])
		// Selecting one role deselects the other by construction: Mode is a
		// single field, so there is no second switch left on behind this.
		if reloadDaemon(cfg.ControlSocket) {
			fmt.Println("daemon reloaded")
		}
		fmt.Println("note: subnets and relay servers are edited through the web admin's System > DHCP page")

	default:
		fatal("usage: gravinet system dhcp <list|mode> [off|server|relay]")
	}
}

func leaseLabel(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%ds", n)
}
