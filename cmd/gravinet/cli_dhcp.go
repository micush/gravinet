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
		// Intent only, deliberately. What is actually running is daemon state:
		// the relay lives inside that process, so a separate CLI process
		// cannot see its sockets and would have to guess. The web admin asks
		// the daemon and reports both (see dhcp_runtime.go); guessing here
		// would put a confident wrong answer on a terminal.
		fmt.Printf("dhcp role (configured): %s\n", mode)
		fmt.Println("  server subnets:")
		if len(d.Subnets) == 0 {
			fmt.Println("    (none)")
		}
		for _, s := range d.Subnets {
			fmt.Printf("    %-10s %-8s %-18s pool=%s-%s router=%s dns=%s lease=%s\n",
				s.Iface, onOff(!s.Disabled), s.Subnet, s.PoolStart, s.PoolEnd,
				orDash(s.Router), orDash(joinComma(s.DNS)), orDash(leaseLabel(s.LeaseSeconds)))
		}
		fmt.Println("  relay links:")
		if len(d.Relay.Links) == 0 {
			fmt.Println("    (none)")
		}
		for _, l := range d.Relay.Links {
			fmt.Printf("    %-10s %-8s servers=%s max_hops=%s\n",
				l.Iface, onOff(!l.Disabled), orDash(joinComma(l.Servers)), hopsLabel(l.MaxHops))
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

// hopsLabel renders a relay hop limit, naming the default rather than printing
// the 0 that stands for it — "max_hops=0" reads as a relay that drops
// everything, which is the opposite of what it means.
func hopsLabel(n int) string {
	if n <= 0 {
		return "4 (default)"
	}
	return fmt.Sprintf("%d", n)
}
