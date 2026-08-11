package webadmin

import (
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
)

// System > Interfaces: a read-only inventory of this host's network
// interfaces, their addresses, and the default gateways in use.
//
// Read-only on purpose, for now. Changing an address is three separate
// problems — applying it, persisting it across a reboot through whichever of
// netplan/NetworkManager/systemd-networkd/rc.conf this host actually uses, and
// not stranding the operator who is connected over the address being changed.
// None of those is served by pretending the first is the whole job, so this
// ships as inventory and says so.
//
// Everything here is derived live from the host at request time. Nothing is
// stored in gravinet's config, which means — deliberately — that restoring a
// config snapshot does not bring host addressing back with it. That gap is
// what prompted this page; closing it is the write half, not this half.

type sysAddr struct {
	CIDR   string `json:"cidr"`
	Family string `json:"family"` // "ipv4" or "ipv6"
	Scope  string `json:"scope"`  // global, link-local, loopback
}

type sysIface struct {
	Name    string    `json:"name"`
	Index   int       `json:"index"`
	MTU     int       `json:"mtu"`
	MAC     string    `json:"mac,omitempty"`
	Up      bool      `json:"up"`
	Running bool      `json:"running"`
	Kind    string    `json:"kind"` // "mesh" for gravinet's own devices, else ""
	Network string    `json:"network,omitempty"`
	Addrs   []sysAddr `json:"addrs"`
	GW4     string    `json:"gw4,omitempty"`
	GW6     string    `json:"gw6,omitempty"`
}

// handleSystemInterfaces returns the inventory.
func (s *Server) handleSystemInterfaces(w http.ResponseWriter, r *http.Request) {
	ifis, err := net.Interfaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// gravinet's own devices are labelled rather than hidden: an operator
	// looking at an interface list wants to see the overlay too, and knowing
	// which network a mesh device belongs to is the useful part.
	meshOf := map[string]string{}
	for _, i := range s.be.Interfaces() {
		if i.Iface != "" {
			meshOf[i.Iface] = i.Name
		}
	}

	gw4, gw6 := defaultGateways()

	out := make([]sysIface, 0, len(ifis))
	for _, ifi := range ifis {
		e := sysIface{
			Name: ifi.Name, Index: ifi.Index, MTU: ifi.MTU,
			MAC:     ifi.HardwareAddr.String(),
			Up:      ifi.Flags&net.FlagUp != 0,
			Running: ifi.Flags&net.FlagRunning != 0,
		}
		if n, ok := meshOf[ifi.Name]; ok {
			e.Kind, e.Network = "mesh", n
		}
		addrs, _ := ifi.Addrs()
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
			// Link-local addresses are not shown. The kernel assigns them,
			// this page cannot change them, and on a v6 host they outnumber
			// the addresses an operator came to look at — so listing them
			// only makes the ones that matter harder to find.
			if addr.IsLinkLocalUnicast() {
				continue
			}
			ones, _ := ipn.Mask.Size()
			sa := sysAddr{CIDR: netip.PrefixFrom(addr, ones).String()}
			if addr.Is4() {
				sa.Family = "ipv4"
			} else {
				sa.Family = "ipv6"
			}
			if addr.IsLoopback() {
				sa.Scope = "loopback"
			} else {
				sa.Scope = "global"
			}
			e.Addrs = append(e.Addrs, sa)
		}
		// Global addresses first: the one an operator is looking for is
		// almost always a global, and loopback should not lead.
		sort.SliceStable(e.Addrs, func(i, j int) bool {
			return scopeRank(e.Addrs[i].Scope) < scopeRank(e.Addrs[j].Scope)
		})
		// A default gateway is a property of the route, not the interface, so
		// it is shown against the interface the route actually leaves by.
		if gw4.iface == ifi.Name {
			e.GW4 = gw4.addr
		}
		if gw6.iface == ifi.Name {
			e.GW6 = gw6.addr
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	writeJSON(w, http.StatusOK, map[string]any{
		"interfaces": out,
		"gw4":        gw4.addr, "gw4_iface": gw4.iface,
		"gw6": gw6.addr, "gw6_iface": gw6.iface,
	})
}

func scopeRank(s string) int {
	if s == "global" {
		return 0
	}
	return 1
}

type gwInfo struct{ addr, iface string }

// defaultGateways reports this host's IPv4 and IPv6 default gateways, each
// with the interface it leaves by. Empty when there is none or the platform
// has no reader — an absent gateway is a fact worth showing plainly, not an
// error to fail the whole inventory over.
func defaultGateways() (gwInfo, gwInfo) {
	return readDefaultGateway(false), readDefaultGateway(true)
}

// prefixHost renders an address without its prefix length, for display.
func prefixHost(cidr string) string {
	if i := strings.IndexByte(cidr, '/'); i > 0 {
		return cidr[:i]
	}
	return cidr
}
