package tui

// The rail: a mirror of internal/webadmin/ui.go's NAV_GROUPS, with the same
// groups in the same order, the same sections in the same order, and the same
// one-line description on each.
//
// This is the third copy of that list in the tree (ui.go's own, the CLI's
// group tables in cmd/gravinet/cli_groups.go, and this), which is exactly the
// arrangement cmd/gravinet/navparity_test.go was written because of: the
// first two drifted apart repeatedly and silently, and the thing that had
// been asserting they matched was a paragraph in a comment. So nav_test.go
// here does what that test does — reads NAV_GROUPS out of ui.go at test time
// and compares group names, section keys, and their order against this table.
// Adding a page to the rail fails this package's tests until it is added
// here too, which is the point.
//
// The descriptions are compared as well, not just the keys. They are the
// text under each page's heading, so a page whose description says something
// the web admin's no longer does is a page telling two different stories to
// two different operators.

import "strings"

// navItem is one page in the rail.
type navItem struct {
	key  string // the section key, matching ui.go's own
	desc string // the tooltip ui.go carries, shown here under the heading
}

// navGroup is one collapsible group of pages.
type navGroup struct {
	name  string
	items []navItem
}

// navGroups mirrors ui.go's NAV_GROUPS. Order is load-bearing in both
// directions: it is the order the rail draws, and it is what nav_test.go
// compares against.
var navGroups = []navGroup{
	{name: "mesh", items: []navItem{
		{"networks", "define overlay networks: subnets, addressing, MTU"},
		{"keys", "cryptographic keys used to authenticate this network\u2019s peers"},
		{"seeds", "bootstrap addresses used to find and reconnect to peers"},
		{"peers", "enable, disable, or ban nodes known on this network"},
		{"bans", "nodes blocked from joining or reconnecting"},
	}},
	{name: "traffic", items: []navItem{
		{"firewall", "rules controlling which traffic is allowed through the tunnel"},
		{"nat", "port forwarding and address translation for this node\u2019s traffic"},
		{"qos", "traffic prioritization and queuing order"},
		{"ipv6ra", "IPv6 router advertisements \u2014 announce this node as a router on a LAN, with DNS"},
		{"bandwidth", "rate limiting per interface"},
		{"routes", "additional subnets redistributed across the mesh"},
		{"bgp", "BGP and BFD configuration, applied to FRR (shown only when vtysh is present on this host)"},
	}},
	{name: "naming", items: []navItem{
		{"dns", "conditional forwarding of specific domains to mesh DNS servers"},
		{"hosts", "custom hostname records advertised to peers"},
	}},
	{name: "monitor", items: []navItem{
		{"metrics", "live CPU, memory, disk, and per-overlay-interface throughput"},
		{"mesh-peers", "live connection health, transport, and session detail for every peer"},
		{"capture", "live packet capture on an overlay interface"},
		{"speedtest", "measure throughput between this node and a managed peer"},
		{"latency", "round-trip time from this host to every other mesh peer"},
		{"route-table", "the live kernel routing table on this host"},
		{"bgp-peers", "live BGP peer sessions reported by FRR (shown only when vtysh is present on this host)"},
		{"l2-peers", "live LLDP/CDP neighbors seen on this host\u2019s interfaces (shown only when lldpd is present on this host)"},
		{"hosts-file", "the live contents of this host\u2019s hosts file"},
		{"dns-state", "what\u2019s actually registered with this host\u2019s OS resolver right now"},
		{"logs", "the daemon\u2019s recent log output"},
	}},
	{name: "system", items: []navItem{
		{"upgrade", "check and apply a new gravinet binary on this node; local only, no peer can trigger this"},
		{"interfaces", "this host\u2019s network interfaces, addresses and default gateways (read-only)"},
		{"resolver", "this host\u2019s hostname and default DNS servers"},
		{"time", "this host\u2019s clock, timezone, and NTP synchronization"},
		{"dhcp", "forward DHCP requests from this node\u2019s LANs to a DHCP server somewhere else"},
		{"snmp", "read-only SNMPv2c monitoring agent (shown only when snmpd is present on this host)"},
		{"lldp", "link-layer discovery (LLDP/CDP) and neighbor status (shown only when lldpd is present on this host)"},
		{"syslog", "forward this host\u2019s syslog to a remote collector (shown only when a supported syslog daemon is present)"},
		{"users", "local OS accounts permitted to sign in to this console"},
		{"config-history", "automatic and manual snapshots of past configurations, with diff and restore"},
		{"power", "restart or shut down this host"},
	}},
	{name: "info", items: []navItem{
		{"readme", "project documentation"},
		{"getting-started", "the full onboarding walkthrough"},
		{"api", "HTTP API reference"},
		{"license", "license information"},
		{"about", "build and host identity"},
	}},
}

// settingsSection is the gear page — reached from the rail's foot in the web
// admin rather than from a group, and the same here. Not part of navGroups
// for the same reason it is not part of NAV_GROUPS.
const settingsSection = "settings"

// sectionKeys returns every section in rail order, settings last. Used by the
// search index and by the tests that walk every page.
func sectionKeys() []string {
	var out []string
	for _, g := range navGroups {
		for _, it := range g.items {
			out = append(out, it.key)
		}
	}
	return append(out, settingsSection)
}

// groupFor reports which group a section belongs to, and "" for settings.
func groupFor(sec string) string {
	for _, g := range navGroups {
		for _, it := range g.items {
			if it.key == sec {
				return g.name
			}
		}
	}
	return ""
}

// descFor returns a section's one-line description.
func descFor(sec string) string {
	for _, g := range navGroups {
		for _, it := range g.items {
			if it.key == sec {
				return it.desc
			}
		}
	}
	if sec == settingsSection {
		return "console, security, and node-wide settings"
	}
	return ""
}

// label is ui.go's label(): the rail's own text for a section. Transcribed
// case for case, including the two rules that are not simple title-casing —
// the acronym set that is fully upper-cased, and the handful of multi-word
// names — because the rail is the thing an operator is reading when they
// compare this to a browser on the next screen over.
func label(sec string) string {
	switch sec {
	case "settings":
		return "Settings"
	case "bandwidth":
		return "Shaping"
	case "route-table":
		return "Route Table"
	case "hosts-file":
		return "Hosts File"
	case "dns-state":
		return "DNS State"
	case "mesh-peers":
		return "Mesh Peers"
	case "routes":
		return "Mesh Routes"
	case "bgp-peers":
		return "BGP Peers"
	case "config-history":
		return "Config History"
	case "l2-peers":
		return "L2 Peers"
	case "getting-started":
		return "Getting Started"
	case "capture":
		return "Packet Capture"
	case "ipv6ra":
		// Rail label only; sectionHeading gives the page the full name, as
		// in ui.go. A rail is narrow, a heading is not.
		return "v6 ra"
	case "nat", "qos", "dns", "bgp", "api", "snmp", "lldp", "dhcp":
		return strings.ToUpper(sec)
	}
	if sec == "" {
		return ""
	}
	return strings.ToUpper(sec[:1]) + sec[1:]
}

// sectionHeading is ui.go's sectionHeading(): label() everywhere except the
// two sections whose rail button is kept short while the page itself gets the
// fuller name.
func sectionHeading(sec string) string {
	switch sec {
	case "ipv6ra":
		return "IPv6 Router Advertisements"
	case "dhcp":
		return "DHCP Relay"
	}
	return label(sec)
}

// sectionVisible is ui.go's sectionVisible(): the pages whose availability
// depends on something being installed on this host, rather than on
// configuration. Same set, same gating, sourced from caps rather than from
// /api/config — see data.go's detectCaps for how each is determined and why
// that is a slightly different question here than in the browser.
func sectionVisible(sec string, c caps) bool {
	switch sec {
	case "bgp", "bgp-peers":
		return c.bgp
	case "ipv6ra":
		return c.ipv6ra
	case "dhcp":
		return c.dhcp
	case "snmp":
		return c.snmp
	case "lldp", "l2-peers":
		return c.lldp
	case "syslog":
		return c.syslog
	}
	return true
}

// defaultSection is where the console opens, matching the web admin's own
// fallback in renderSection.
const defaultSection = "networks"
