package webadmin

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sort"
	"strings"

	"gravinet/internal/config"
)

// Preflight for the DHCP relay, the same idea as radvd_preflight.go and for
// the same reason: an apply can succeed at every step it checks and still
// leave a host relaying for nobody.
//
// The condition that matters is the interface address. The interface's own
// IPv4 address is the giaddr the relay stamps on what it forwards, and that
// stamp is how the upstream server picks which subnet to answer from — so an
// interface without one cannot relay at all.
//
// It is a property of the host rather than of the configuration, which is why
// it is checked at apply time and again on every page load rather than
// validated once on save. An interface loses its address long after the row
// naming it was written.

// dhcpSupported gates the System > DHCP nav item, the way ipv6RASupported
// gates Traffic > IPv6 RA.
//
// Linux only, and this is the relay's own limit rather than a daemon's: it
// binds a wildcard socket and confines it to one interface with
// SO_BINDTODEVICE, which the BSDs spell differently and Windows not at all.
// See internal/dhcrelay/listen_other.go for why guessing the arrival link is
// worse than refusing — a relay that guessed would hand clients addresses from
// another LAN's subnet, which does not look like a failure from either end.
func dhcpSupported() bool { return runtime.GOOS == "linux" }

// dhcpMeshIfaces is the set of gravinet's own devices, which the picker does
// not offer. Shared with the VLAN page so the two cannot disagree about what
// counts as one.
func (s *Server) dhcpMeshIfaces() []string {
	var out []string
	for _, i := range s.be.Interfaces() {
		if i.Iface != "" {
			out = append(out, i.Iface)
		}
	}
	sort.Strings(out)
	return out
}

// dhcpIfaceAddrs returns an interface's global IPv4 addresses, or an error
// describing why it cannot carry DHCP at all.
//
// A nil slice and a nil error mean the addresses could not be read at all, and
// is distinct from an empty one, which means they were read and none of them
// are usable IPv4. The caller below branches on exactly that difference — the
// first is a lookup that failed and should stay quiet, the second is a real
// fault worth reporting. Returning the zero value for both is what made those
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

// dhcpProblems maps each interface that cannot relay to the reason. Keyed by
// interface so the page can put each reason under the row it belongs to.
//
// Only the links actually in service are checked. A parked link, or a whole
// relay configuration sitting unused while the mode is off, is not supposed to
// be doing anything — reporting that it is not would be handing the operator
// their own request back as a fault.
func dhcpProblems(c config.DHCPConfig) map[string]string {
	out := map[string]string{}
	for _, l := range c.EnabledLinks() {
		iface := strings.TrimSpace(l.Iface)
		if p := dhcpRelayProblem(iface); p != "" {
			out[iface] = p
		}
	}
	return out
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
