package webadmin

import (
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
)

// System > Interfaces: this host's network interfaces, their addresses, the
// default gateways in use, and — for the interfaces gravinet manages — where
// each family's addresses come from.
//
// The addresses, gateways and MTUs are read live from the host at request time,
// because they are facts about the interface rather than about gravinet. The
// modes are the exception and come from the configuration: an interface holding
// a static-looking address is indistinguishable from one holding a lease by
// looking at it, so the only place the answer exists is the record of what
// gravinet was asked to do. An interface with no record reports no mode, which
// is how the page distinguishes "gravinet does not manage this" from "static".

type sysAddr struct {
	CIDR   string `json:"cidr"`
	Family string `json:"family"` // "ipv4" or "ipv6"
	Scope  string `json:"scope"`  // global, link-local, loopback
	// Mode is where this one address came from, and it is per address rather
	// than per family because a family's mode does not settle it. Under a
	// static family, an address gravinet records is "static" and one it does
	// not is "unmanaged" — a lease that has not gone away yet, or something
	// set by hand. Painting the family's mode onto every address of that
	// family labelled a leftover DHCP address "static", which is the one
	// question the tag exists to answer.
	//
	// Empty on an interface with no record, and on link-local and loopback.
	Mode string `json:"mode,omitempty"`
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
	// Mode4/Mode6 are empty on an interface gravinet does not manage. Empty is
	// not "static": a record that says static is a decision someone made, and
	// no record at all is an interface nobody has touched through gravinet.
	Mode4 string `json:"mode4,omitempty"`
	Mode6 string `json:"mode6,omitempty"`
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

	// A failure to read the configuration is not a reason to fail the
	// inventory: the addresses are the point of this page and they do not come
	// from there. The modes are simply absent, which reads as unmanaged.
	modes := map[string]config.HostIface{}
	if cfg, err := config.Load(s.configPath); err == nil {
		for _, h := range cfg.HostInterfaces {
			modes[h.Iface] = h
		}
	}

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
		// recorded is the set of addresses gravinet has actually been told to
		// put on this interface, so a static family can distinguish its own
		// addresses from whatever else is up.
		recorded := map[netip.Prefix]bool{}
		h, managed := modes[ifi.Name]
		if managed {
			// Reported through Or, so a record written before modes existed
			// says "static" rather than nothing — which is what it has always
			// meant and what the interface is actually doing.
			e.Mode4 = string(h.Mode4.Or(hostnet.ModeStatic))
			e.Mode6 = string(h.Mode6.Or(hostnet.ModeStatic))
			for _, a := range h.Addrs {
				if p, err := netip.ParsePrefix(strings.TrimSpace(a)); err == nil {
					recorded[netip.PrefixFrom(p.Addr().Unmap(), p.Bits())] = true
				}
			}
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
			if managed && sa.Scope == "global" {
				fam := hostnet.Mode(e.Mode4)
				if addr.Is6() {
					fam = hostnet.Mode(e.Mode6)
				}
				switch {
				case !fam.IsStatic():
					// The family takes its addressing from the network, so
					// this address did come from there.
					sa.Mode = string(fam)
				case recorded[netip.PrefixFrom(addr, ones)]:
					sa.Mode = string(hostnet.ModeStatic)
				default:
					sa.Mode = "unmanaged"
				}
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
