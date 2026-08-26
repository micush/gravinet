package webadmin

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"gravinet/internal/config"
)

// Preflight for DHCP, the same idea as radvd_preflight.go and for the same
// reason: an apply can succeed at every step it checks and still leave a host
// serving nobody.
//
// The condition that matters here is the interface address. Kea decides which
// scope a request belongs to by matching the receiving interface's address
// against the configured subnets, so an interface whose address is outside the
// subnet it is configured to serve gets a server that starts, runs, logs
// nothing unusual, and never answers. The relay has the same shape of problem
// one field over: the interface's own IPv4 address is the giaddr it stamps on
// forwarded requests, and an interface without one cannot relay at all.
//
// Both are properties of the host rather than of the configuration, which is
// why they are checked at apply time and again on every page load rather than
// validated once on save. An interface loses its address long after the row
// naming it was written.

// dhcpIfaceAddrs returns an interface's global IPv4 addresses, or an error
// describing why it cannot carry DHCP at all.
//
// A nil slice and a nil error mean the addresses could not be read at all, and
// is distinct from an empty one, which means they were read and none of them
// are usable IPv4. Callers below branch on exactly that difference — the first
// is a lookup that failed and should stay quiet, the second is a real fault
// worth reporting. Returning the zero value for both is what made those
// reports unreachable until v945.
func dhcpIfaceAddrs(name string) ([]netip.Prefix, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("no interface by that name on this host")
	}
	if ifi.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("interface is down")
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		// The interface is there and its addresses could not be read.
		// Inventing a fault out of a failed lookup would be worse than
		// staying quiet and letting the daemon be the judge.
		return nil, nil
	}
	return v4Prefixes(addrs), nil
}

// v4Prefixes keeps the addresses of an interface that DHCP can actually use:
// IPv4, and neither loopback nor link-local. Never nil, so the caller can tell
// "read them, there are none" from "could not read them".
//
// Shared with the prefill in dhcp_prefill.go rather than copied, so the set of
// addresses the page suggests from is by construction the same set the
// preflight judges the result against. Two filters that drift apart would
// produce a form that fills itself in and then complains about what it filled.
func v4Prefixes(addrs []net.Addr) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !addr.Is4() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
			continue
		}
		ones, _ := ipn.Mask.Size()
		out = append(out, netip.PrefixFrom(addr, ones))
	}
	return out
}

// dhcpProblems maps each interface that cannot do its configured job to the
// reason. Keyed by interface so the page can put each reason under the row it
// belongs to.
//
// Only the active mode is checked. A parked subnet, or a relay configuration
// sitting unused while the node serves, is not supposed to be doing anything —
// reporting that it is not would be handing the operator their own request
// back as a fault.
func dhcpProblems(c config.DHCPConfig) map[string]string {
	out := map[string]string{}
	switch c.Mode {
	case config.DHCPServer:
		for _, s := range c.EnabledSubnets() {
			iface := strings.TrimSpace(s.Iface)
			// First reason wins. Several subnets legitimately share one
			// interface now that relayed scopes exist, and a map keyed by
			// interface can hold one reason for it — last-write-wins would
			// make which of two faults an operator sees depend on the order
			// the rows happen to be stored in.
			if _, have := out[iface]; have {
				continue
			}
			if p := dhcpServerProblem(s, iface); p != "" {
				out[iface] = p
			}
		}
	case config.DHCPRelay:
		for _, l := range c.EnabledLinks() {
			iface := strings.TrimSpace(l.Iface)
			if p := dhcpRelayProblem(iface); p != "" {
				out[iface] = p
			}
		}
	}
	return out
}

func dhcpServerProblem(s config.DHCPSubnet, iface string) string {
	addrs, err := dhcpIfaceAddrs(iface)
	if err != nil {
		return fmt.Sprintf("%s: %v, so nothing is served on it", iface, err)
	}
	if addrs == nil {
		return ""
	}
	// A relayed subnet is checked against a different condition, because the
	// condition below is one it is *supposed* to fail. Kea does not match a
	// forwarded request by the receiving interface's address, so a remote
	// LAN's scope has no business being addressed on the link its relay
	// reaches us over — running the attached check here would put a red row
	// under every correctly configured remote subnet on the node.
	//
	// What still has to hold is that the interface can receive the forwarded
	// unicast at all, which needs an IPv4 address on it — any address, not
	// one inside the subnet being served.
	if s.Relayed() {
		if len(addrs) == 0 {
			return fmt.Sprintf("%s has no IPv4 address, so relayed requests for %s have nowhere to arrive — a relay forwards to an address on this host, not to a broadcast",
				iface, strings.TrimSpace(s.Subnet))
		}
		return ""
	}
	want, perr := netip.ParsePrefix(strings.TrimSpace(s.Subnet))
	if perr != nil {
		return ""
	}
	for _, have := range addrs {
		if want.Masked().Contains(have.Addr()) {
			return ""
		}
	}
	if len(addrs) == 0 {
		return fmt.Sprintf("%s has no IPv4 address, so Kea cannot match it to subnet %s and will not answer on it", iface, want.Masked())
	}
	var got []string
	for _, a := range addrs {
		got = append(got, a.Addr().String())
	}
	return fmt.Sprintf("%s carries %s, which is outside the subnet %s it is set to serve — Kea matches a request to a scope by the receiving interface's address, so it will start and never answer here",
		iface, strings.Join(got, ", "), want.Masked())
}

func dhcpRelayProblem(iface string) string {
	addrs, err := dhcpIfaceAddrs(iface)
	if err != nil {
		return fmt.Sprintf("%s: %v, so nothing is relayed from it", iface, err)
	}
	if addrs != nil && len(addrs) == 0 {
		return fmt.Sprintf("%s has no IPv4 address to use as the relay address (giaddr), so requests from it cannot be forwarded", iface)
	}
	return ""
}

// dhcpProblemNote renders the preflight for the apply note, or "" when there
// is nothing wrong. Sorted, so a note assembled from a map does not reorder
// itself between two saves that changed nothing. The trailing separator is
// noteworthy's convention.
func dhcpProblemNote(c config.DHCPConfig) string {
	probs := dhcpProblems(c)
	if len(probs) == 0 {
		return ""
	}
	names := make([]string, 0, len(probs))
	for n := range probs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(probs[n])
		b.WriteString("; ")
	}
	return b.String()
}

// servableSubnets drops the subnets naming an interface this host does not
// have, and returns their interface names alongside the trimmed config.
//
// Kea refuses an entire configuration for one missing interface — not the
// subnet, the file:
//
//	subnet configuration failed: Specified network interface name enp99s0
//	for subnet 192.168.50.0/24 is not present in the system
//
// so a NIC renamed by a kernel upgrade, or a config restored onto different
// hardware, stops DHCP on every other LAN this node serves. That is the exact
// failure renderKea's own rule is written against: one bad subnet must not
// take the leases for every other link down with it.
//
// A *down* interface is left in place. Kea accepts one and starts, and an
// operator whose link is down for a minute should not come back to a subnet
// silently missing from the running config. Absence is the condition that
// breaks the file, so absence is the condition checked.
//
// The names come back rather than being logged here, so the apply can put them
// in the note. A subnet quietly not served is the thing to avoid — dropping it
// is only the better of two bad outcomes if it is also reported.
func servableSubnets(c config.DHCPConfig) (config.DHCPConfig, []string) {
	out := c
	out.Subnets = make([]config.DHCPSubnet, 0, len(c.Subnets))
	var dropped []string
	for _, s := range c.Subnets {
		name := strings.TrimSpace(s.Iface)
		if s.Disabled || name == "" {
			out.Subnets = append(out.Subnets, s)
			continue
		}
		if _, err := net.InterfaceByName(name); err != nil {
			dropped = append(dropped, name)
			continue
		}
		out.Subnets = append(out.Subnets, s)
	}
	return out, dropped
}
