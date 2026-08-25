package webadmin

import (
	"net"
	"net/netip"
	"sort"
	"strings"

	"gravinet/internal/service"
)

// Prefill for System > DHCP's subnet editor.
//
// Everything a served subnet needs is already true of the interface serving
// it: the CIDR is the interface's own prefix, the gateway is almost always the
// interface's own address, and the pool is whatever is left over. Making an
// operator retype all of it from the addresses on the next page is how a pool
// ends up one octet off the subnet it belongs to — which is precisely the
// failure dhcp_preflight.go exists to catch after the fact.
//
// So the page fills the row in when an interface is chosen, and every field
// stays editable. A suggestion is a starting point, not a decision: this
// computes what is almost certainly right and then gets out of the way.
//
// It is computed here rather than in the browser for two reasons. The
// arithmetic has edges — a /29, an interface addressed in the middle of its
// own subnet, a /31 with no host range at all — and edges belong somewhere
// they can be tested. And the addresses are the *node's*, so on a managed peer
// they have to come from that node's reply; deriving them client-side would
// quietly describe whichever host the browser is pointed at.

// dhcpReserve is how many addresses are held back at each end of a subnet, and
// on each side of the router, when a pool is suggested.
//
// Ten is a "few" that leaves room for the things that turn up later on a
// segment an operator has just started serving — a second gateway, a printer,
// a switch's management address — without anyone having to shrink a live pool
// to make space. It is a starting point and the field is editable; the value
// only has to be defensible, not correct for every network.
const dhcpReserve = 10

// dhcpSuggestion is the prefill for one interface. Pool may be empty while the
// rest is not: a /31 has an address and a prefix but no host range to hand
// out, and suggesting a pool there would mean suggesting an invalid one.
type dhcpSuggestion struct {
	Subnet    string `json:"subnet"`
	PoolStart string `json:"pool_start,omitempty"`
	PoolEnd   string `json:"pool_end,omitempty"`
	Router    string `json:"router"`
}

// dhcpSuggestions maps interface name to its prefill. Interfaces with no
// usable IPv4 address are absent rather than present and empty, so the page
// can tell "nothing to suggest" from "suggest nothing" and leave the row alone
// instead of blanking it.
//
// skip carries the mesh devices, which are excluded for the same reason the
// picker hides them: a suggestion for a link that can never be served is an
// invitation to serve it.
func dhcpSuggestions(skip map[string]bool) map[string]dhcpSuggestion {
	out := map[string]dhcpSuggestion{}
	ifis, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifis {
		if skip[ifi.Name] {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		// The first usable IPv4 address wins on a multi-homed interface.
		// Picking one and letting it be edited is better than picking none:
		// a second address on a LAN interface is usually a secondary, and
		// the operator can see which one landed in the field.
		for _, p := range v4Prefixes(addrs) {
			if s, ok := suggestDHCPSubnet(p); ok {
				out[ifi.Name] = s
				break
			}
		}
	}
	return out
}

// suggestDHCPSubnet derives a subnet row from one interface address.
//
// The pool runs from dhcpReserve past the network address to dhcpReserve short
// of the broadcast address, so both ends keep a handful of addresses back for
// whatever needs a static one later — the request that starts every one of
// these pages is somebody who has run out of room below .100. The margin
// shrinks on a small subnet rather than giving up, and is dropped entirely
// when there is no host range to divide.
//
// The interface's own address is kept out of the pool along with a margin
// either side of it. A gateway inside its own pool is rejected by
// DHCPSubnet.Validate, and being rejected by the validator is the one thing a
// prefill must never be: the form would be refusing what it had just written.
func suggestDHCPSubnet(iface netip.Prefix) (dhcpSuggestion, bool) {
	addr := iface.Addr().Unmap()
	if !addr.Is4() || !iface.IsValid() {
		return dhcpSuggestion{}, false
	}
	// A subnet is what an interface's prefix means; an address with host bits
	// set is the ordinary case here rather than an error.
	network := iface.Masked()
	s := dhcpSuggestion{Subnet: network.String(), Router: addr.String()}

	bits := network.Bits()
	if bits > 30 {
		// /31 and /32 address a point-to-point link or a single host. There
		// is no pool to be had, and the subnet and router are still worth
		// filling in.
		return s, true
	}
	size := uint32(1) << uint(32-bits)
	base := v4ToU32(network.Addr())
	broadcast := base + size - 1

	margin := uint32(dhcpReserve)
	if max := (size - 1) / 2; margin > max {
		margin = max
	}
	if margin == 0 {
		return s, true
	}
	lo, hi := base+margin, broadcast-margin
	if lo > hi {
		return s, true
	}

	// Split around the router when it lands inside, and keep the wider half.
	// The margin applies here too: the addresses next to a gateway are the
	// ones somebody reaches for first when they need a fixed address.
	if r := v4ToU32(addr); r >= lo && r <= hi {
		aOK, bOK := r >= margin && r-margin >= lo, r+margin <= hi
		switch {
		case aOK && bOK:
			if (r-margin)-lo >= hi-(r+margin) {
				hi = r - margin
			} else {
				lo = r + margin
			}
		case aOK:
			hi = r - margin
		case bOK:
			lo = r + margin
		default:
			return s, true // nothing left either side of the gateway
		}
	}
	s.PoolStart, s.PoolEnd = u32ToV4(lo).String(), u32ToV4(hi).String()
	return s, true
}

// v4ToU32 and u32ToV4 convert between an IPv4 address and the integer the
// pool arithmetic above runs on. netip.Addr has Next and Prev, but stepping a
// ten-address margin one call at a time reads as a loop whose purpose is to be
// counted rather than as the addition it is.
func v4ToU32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func u32ToV4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// systemDNSv4 is what this host resolves through, filtered down to what is
// worth handing a client.
//
// Loopback is dropped, and that is the whole reason this is not just the
// resolver list passed through. A host that resolves through 127.0.0.1 is
// pointing at a resolver running on itself; offered to a client over DHCP the
// same address means the client's *own* loopback, so the one setting that
// looks most like working DNS is the one guaranteed not to be. Better to hand
// over an empty field an operator has to fill than an address that resolves
// nothing and reads as correct.
func systemDNSv4() []string {
	return filterDNSv4(service.HostResolver().DNSServers)
}

func filterDNSv4(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		a = a.Unmap()
		// IPv4 only: option 6 carries v4 addresses, and a v6 resolver reaches
		// clients through the router advertisement instead (Traffic > IPv6 RA).
		if !a.Is4() || a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() {
			continue
		}
		if k := a.String(); !seen[k] {
			seen[k], out = true, append(out, k)
		}
	}
	return out
}

// dhcpMeshIfaces is the set of gravinet's own devices, which neither the
// picker nor the prefill offers. Shared by the two so they cannot disagree.
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
