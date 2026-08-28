package webadmin

import (
	"fmt"
	"strings"

	"gravinet/internal/config"
)

// What the relay is actually doing, which is not the same question as whether
// it was switched on.
//
// The pill by the page title is a control: it records what the operator asked
// for, and it has to keep showing that or there would be no way to enable the
// relay before configuring it. So a pill cannot be driven from reality without
// destroying its own purpose — which is why what is actually running is
// reported beside it rather than through it.
//
// The problem was that reality was reported nowhere at all. Every one of these
// leaves the stored mode saying one thing and the host doing another, silently:
//
//   - the relay enabled with no link that has both an interface and an upstream
//     server: EnabledLinks is empty and the relay never starts
//   - the relay enabled but no configured interface could be bound
//   - a link losing its address after the apply
//
// So this asks the relay object itself for the links it actually bound. It is
// deliberately not derived from the config, because the config is the thing
// being checked against.

// dhcpRunning is what the node is doing about DHCP right now.
type dhcpRunning struct {
	// Role is the role actually in service: "relay", or "" for nothing. Not
	// the configured mode.
	//
	// Still a role rather than a bool, because the page renders it as a
	// sentence beside the pill and "relaying on eth1" is what that sentence
	// has to say. It has one value where it used to have two.
	Role string `json:"role"`
	// Ifaces are the interfaces the relay is bound to.
	Ifaces []string `json:"ifaces,omitempty"`
	// Why explains a mismatch, and is empty when the node is doing what was
	// selected. Written for somebody looking at a page that says one thing
	// while their clients get no address.
	Why string `json:"why,omitempty"`
}

// dhcpRuntime reports what is running and, when that differs from what the
// pill is set to, why.
func dhcpRuntime(c config.DHCPConfig) dhcpRunning {
	if c.Mode != config.DHCPRelay {
		// Off. Nothing running is the selected outcome, so there is nothing to
		// explain.
		return dhcpRunning{}
	}
	if live := dhcpRelay.Listening(); len(live) > 0 {
		return dhcpRunning{Role: "relay", Ifaces: live}
	}
	return dhcpRunning{Why: whyNotRelaying(c)}
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
