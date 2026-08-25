package webadmin

import (
	"fmt"
	"strings"

	"gravinet/internal/config"
)

// What DHCP is actually doing, which is not the same question as what role was
// last selected.
//
// Each card's pill is a control: it records what the operator asked for, and
// it has to keep showing that or there would be no way to enable a role before
// configuring it. So a pill cannot be driven from reality without destroying
// its own purpose — which is why what is actually running is reported beside
// it rather than through it.
//
// The problem was that reality was reported nowhere at all. Every one of these
// leaves the stored mode saying one thing and the host doing another, silently:
//
//   - the server enabled with no enabled subnet: the apply stops Kea, because Kea
//     refuses to start with no subnet4 and would otherwise crash-loop
//   - the server enabled but every subnet names an interface this host does not
//     have: servableSubnets drops them all, and the same stop happens
//   - the server enabled and the Kea install failed: nothing to run
//   - the relay enabled with no link that has both an interface and an upstream
//     server: EnabledLinks is empty and the relay never starts
//   - the relay enabled but no configured interface could be bound
//   - anything at all changing outside gravinet: a hand-typed `systemctl stop`,
//     a package upgrade, a link losing its address after the apply
//
// So this asks the host: systemd for the unit, and the relay object itself for
// the links it actually bound. It is deliberately not derived from the config,
// because the config is the thing being checked against.

// dhcpRunning is what the node is doing about DHCP right now.
type dhcpRunning struct {
	// Role is the role actually in service: "server", "relay", or "" for
	// nothing. Not the configured mode.
	Role string `json:"role"`
	// Ifaces are the interfaces that role is in service on.
	Ifaces []string `json:"ifaces,omitempty"`
	// Why explains a mismatch, and is empty when the node is doing what was
	// selected. Written for somebody looking at a page that says one thing
	// while their clients get no address.
	Why string `json:"why,omitempty"`
}

// dhcpRuntime reports what is running and, when that differs from what the
// cards are set to, why.
func dhcpRuntime(c config.DHCPConfig) dhcpRunning {
	switch c.Mode {
	case config.DHCPServer:
		// The subnets Kea would actually be given: enabled, and naming an
		// interface this host has. Same filter the apply renders from, so the
		// two cannot disagree about what "served" means.
		served, dropped := servableSubnets(c)
		var ifaces []string
		for _, s := range served.EnabledSubnets() {
			ifaces = append(ifaces, strings.TrimSpace(s.Iface))
		}
		if keaActive() && len(ifaces) > 0 {
			return dhcpRunning{Role: "server", Ifaces: ifaces}
		}
		return dhcpRunning{Why: whyNotServing(c, ifaces, dropped)}

	case config.DHCPRelay:
		live := dhcpRelay.Listening()
		// A Kea unit running while this node relays is the one state the mode
		// field is supposed to make impossible, so it is reported ahead of
		// everything else — including ahead of the relay working fine, which
		// it may well be. Clients on the link get two servers racing and take
		// whichever reply lands first, so "the relay is up" is true and
		// useless on its own here.
		if keaActive() {
			return dhcpRunning{
				Role:   "relay",
				Ifaces: live,
				Why: "the relay is enabled, but a Kea DHCP server is also running on this host — clients on these links will get answers from both, whichever arrives first. " +
					"Enabling and disabling the server card, or restarting gravinet, will stop and disable it.",
			}
		}
		if len(live) > 0 {
			return dhcpRunning{Role: "relay", Ifaces: live}
		}
		return dhcpRunning{Why: whyNotRelaying(c)}
	}
	// Off. Nothing running is the selected outcome, so there is normally no
	// mismatch to report — but a Kea unit left enabled by an earlier apply is
	// still a server this node is running while its role says it does nothing.
	if keaActive() {
		return dhcpRunning{
			Role: "server",
			Why: "the DHCP server card is disabled, but a Kea DHCP server is still running on this host — it is handing out leases this page does not manage. " +
				"Restarting gravinet will stop and disable it.",
		}
	}
	return dhcpRunning{}
}

// whyNotServing explains a node set to serve that is not serving.
func whyNotServing(c config.DHCPConfig, ifaces, dropped []string) string {
	if !keaInstalled() {
		return "the server is enabled, but the Kea DHCPv4 server is not installed on this host — saving a subnet installs it"
	}
	if len(ifaces) == 0 {
		if len(dropped) > 0 {
			return fmt.Sprintf("the server is enabled, but every configured subnet names an interface this host does not have (%s), so nothing is being served",
				strings.Join(dropped, ", "))
		}
		if len(c.Subnets) == 0 {
			return "the server is enabled, but no subnet is configured yet, so nothing is being served"
		}
		return "the server is enabled, but every subnet is disabled, so nothing is being served"
	}
	return fmt.Sprintf("the server is enabled and %s configured, but the Kea service is not running — check `journalctl -u %s`",
		countLabel(len(ifaces), "subnet is", "subnets are"), keaUnit())
}

// whyNotRelaying explains a node set to relay that is not relaying.
func whyNotRelaying(c config.DHCPConfig) string {
	links := c.Relay.Links
	if len(links) == 0 {
		return "the relay is enabled, but no relay link is configured yet, so nothing is being relayed"
	}
	enabled := c.EnabledLinks()
	if len(enabled) == 0 {
		// Distinguish the two half-written states, because the fix differs:
		// one needs the state tag toggled, the other needs a server typed in.
		anyOn := false
		for _, l := range links {
			if !l.Disabled {
				anyOn = true
				break
			}
		}
		if !anyOn {
			return "the relay is enabled, but every relay link is disabled, so nothing is being relayed"
		}
		return "the relay is enabled, but no enabled link has an upstream server to forward to, so nothing is being relayed"
	}
	var names []string
	for _, l := range enabled {
		names = append(names, strings.TrimSpace(l.Iface))
	}
	return fmt.Sprintf("the relay is enabled and %s enabled, but the relay is not listening on %s — the interface may have no IPv4 address to use as the relay address (giaddr), or may not exist on this host",
		countLabel(len(names), "link is", "links are"), strings.Join(names, ", "))
}

// countLabel renders "1 link is" / "3 links are" without the caller having to
// carry the plural around.
func countLabel(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
