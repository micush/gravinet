package webadmin

import (
	"net"
	"net/netip"
	"strings"

	"gravinet/internal/config"
)

// Kea needs a socket on the interface a reply leaves by, not only on the one
// the query arrived on.
//
// v972 gave a relayed scope the udp socket type so the forwarded DISCOVER
// would be *received*: a udp socket matches on address rather than on arrival
// interface, so a unicast to eth1's address arriving over the mesh reaches
// Kea. That half works, and the notes written at the time claimed the overlay
// device therefore never had to be named. It did not go on to check the reply.
//
// Sending is resolved by route, not by the socket the query came in on. The
// DHCPOFFER goes to giaddr, and giaddr is by definition on the far side of the
// relay, so it egresses whatever interface reaches the relay — here the mesh.
// Kea then looks for one of its own sockets on that interface, finds none, and
// drops the reply:
//
//	DHCP4_PACKET_SEND ... trying to send packet DHCPOFFER (type 2)
//	    from 10.1.1.1:67 to 10.4.4.1:67 on interface eth1
//	DHCP4_PACKET_SEND_FAIL ... failed to send DHCPv4 packet:
//	    Interface mesh0/19 does not have any suitable IPv4 sockets open.
//
// Observed on the same four-node lab v972 was reported from, with a config
// v976 renders correctly: subnet selected, lease chosen, reply discarded. The
// end-to-end test asserts DHCP4_LEASE_ADVERT, which is the line immediately
// before the failure, so it passed throughout.
//
// So the egress interface has to be in interfaces-config. Confirmed by adding
// "mesh0" to that list by hand on the lab node, which made the relayed scope
// work end to end.
//
// This is not the operator naming a mesh device, and refuseMeshIface still
// refuses that. It is gravinet opening a reply socket on the link a configured
// relay is already reached over. What that socket can do is bounded: it is a
// udp socket, so it receives no broadcasts, and no subnet4 entry names the
// overlay or carries an overlay giaddr, so a request arriving on it selects no
// scope and is dropped. It is a way out, not a way in.
func relayReplyIfaces(c config.DHCPConfig) (ifaces []string, unresolved []string) {
	seen := map[string]bool{}
	for _, s := range c.EnabledSubnets() {
		if !s.Relayed() {
			continue
		}
		for _, addr := range s.RelayAddrs() {
			name, ok := egressIfaceFor(addr)
			if !ok {
				unresolved = append(unresolved, addr)
				continue
			}
			if !seen[name] {
				seen[name] = true
				ifaces = append(ifaces, name)
			}
		}
	}
	return ifaces, unresolved
}

// egressIfaceFor reports which interface this host would send to addr from.
//
// Done by asking the kernel for a route the way anything else would — a
// connected UDP socket, which sends nothing and only fixes the source address
// — rather than by reading a routing table per platform. The source address it
// picks is the one the route selected, and the interface owning that address
// is the one the packet leaves by. That is the same answer `ip route get`
// gives, without a netlink implementation for each OS gravinet runs on.
//
// The route is read at apply time and baked into a file. A later route change
// that moves a relay onto a different link leaves Kea holding a socket on the
// old one, and the fix is another apply. Acceptable because a relay's path is
// a deployment property, not a thing that flaps, and the alternative is
// watching the routing table to rewrite a config and bounce a daemon.
func egressIfaceFor(relayAddr string) (string, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(relayAddr))
	if err != nil || !ip.Is4() {
		return "", false
	}
	// Port 67 is arbitrary here: nothing is sent, and the route does not
	// depend on it. It matches the traffic this is being asked about.
	conn, err := net.Dial("udp4", net.JoinHostPort(ip.String(), "67"))
	if err != nil {
		// No route to the relay at all. Reported rather than guessed at:
		// picking an interface here would produce a config that looks right
		// and still drops every reply.
		return "", false
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil {
		return "", false
	}
	src, ok := netip.AddrFromSlice(local.IP.To4())
	if !ok {
		return "", false
	}
	return ifaceOwning(src)
}

// ifaceOwning returns the name of the interface configured with addr.
func ifaceOwning(addr netip.Addr) (string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil {
				continue
			}
			if got, ok := netip.AddrFromSlice(v4); ok && got == addr {
				return iface.Name, true
			}
		}
	}
	return "", false
}
