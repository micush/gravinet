package main

import (
	"fmt"

	"gravinet/internal/config"
)

// cmdSystemDHCP is the CLI leaf for System > DHCP. It exists for nav parity —
// every web-admin page has a CLI counterpart, and a page reachable only in a
// browser is a page an operator cannot script or inspect over ssh.
//
// Read and switch only, deliberately, the same split cmdIPv6RA draws. What is
// worth having on a terminal at 3am is seeing what this node is set to do and
// being able to stop it; a link needs an interface and at least one upstream
// server, and reproducing the editor as flags would be a second, weaker
// implementation of a form that already validates them.
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
		fmt.Printf("dhcp relay (configured): %s\n", mode)
		fmt.Println("  relay links:")
		if len(d.Relay.Links) == 0 {
			fmt.Println("    (none)")
		}
		for _, l := range d.Relay.Links {
			fmt.Printf("    %-10s %-8s servers=%s max_hops=%s\n",
				l.Iface, onOff(!l.Disabled), orDash(joinComma(l.Servers)), hopsLabel(l.MaxHops))
		}
		// Parked links are listed with the rest, because they are stored with
		// the rest: switching a link off for an afternoon does not discard
		// where it forwarded to. The state column is what says which is live.
		if d.RetiredServerMode() {
			fmt.Println()
			fmt.Println("note: this config still has this node serving DHCP through Kea, a role removed in v988.")
			fmt.Println("      gravinet has not touched the Kea service — if it was running it still is, still")
			fmt.Println("      enabled at boot, and serving a config nothing manages now. The served subnets are")
			fmt.Println("      in this node's config history if they are worth recreating elsewhere.")
		}

	case "mode":
		if len(rest) == 0 {
			fatal("usage: gravinet system dhcp mode <off|relay>")
		}
		m := config.DHCPMode(rest[0])
		if rest[0] == "off" {
			m = config.DHCPOff
		}
		// Checked before it is stored, not after. cfg.Validate would quietly
		// fold the retired server mode to off, so validating only on the way
		// out would answer somebody asking for "server" with a silent success.
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
		fmt.Printf("dhcp relay set to %s\n", rest[0])
		if reloadDaemon(cfg.ControlSocket) {
			fmt.Println("daemon reloaded")
		}
		fmt.Println("note: relay links are edited through the web admin's System > DHCP page")

	default:
		fatal("usage: gravinet system dhcp <list|mode> [off|relay]")
	}
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
