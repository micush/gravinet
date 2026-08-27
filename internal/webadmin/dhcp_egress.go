package webadmin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
// suggestRelayIface is the interface the page prefills into a relayed row's
// iface column: the one this host would answer that relay across.
//
// A suggestion, not a decision, exactly like the subnet and pool the attached
// rows get from dhcp_prefill.go. The operator can overwrite it, including with
// a mesh device, which for a relayed row is often the right answer and is no
// longer refused on save.
//
// Blank when the relays disagree with each other. A row can carry several
// giaddrs and nothing requires them to be reached the same way; one column
// cannot express two links, and guessing one of them would produce a row that
// answers some of its relays and silently drops the rest. Better to leave it
// empty and let relayIfaceNote say what is wrong once it is filled in.
func suggestRelayIface(relays []string) string {
	name := ""
	for _, addr := range relays {
		got, ok := egressIfaceFor(addr)
		if !ok {
			return ""
		}
		if name == "" {
			name = got
			continue
		}
		if name != got {
			return ""
		}
	}
	return name
}

// relayIfaceNote reports relayed scopes whose iface column is not the link
// this host would actually answer their relay across.
//
// A warning rather than a refusal, and reported rather than corrected. The
// route is this host's opinion at this moment; the operator may know better,
// may be configuring a link that is not up yet, or may be about to change the
// routing. What is not acceptable is the v978 failure mode, where the page
// showed a configuration that looked exactly right and Kea logged an offer it
// then discarded, with nothing anywhere to connect the two.
func relayIfaceNote(c config.DHCPConfig) string {
	var wrong []string
	for _, sub := range c.EnabledSubnets() {
		if !sub.Relayed() {
			continue
		}
		named := strings.TrimSpace(sub.Iface)
		for _, addr := range sub.RelayAddrs() {
			got, ok := egressIfaceFor(addr)
			if !ok {
				wrong = append(wrong, fmt.Sprintf("no route to relay %s, so %s cannot be answered from here", addr, sub.Subnet))
				continue
			}
			if got != named {
				wrong = append(wrong, fmt.Sprintf("%s relays via %s but its interface column says %s, and Kea can only reply from an interface it listens on",
					sub.Subnet, got, named))
			}
		}
	}
	if len(wrong) == 0 {
		return ""
	}
	return strings.Join(dedupe(wrong), "; ") + "; "
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

// handleDHCPRelayIface answers "which interface would this node reach these
// relay agents over?" for the DHCP editor, which prefills the answer into a
// relayed row's interface column.
//
// Server-side because the routing table is the *node's*. On a managed peer the
// browser is not on that host, and a lookup done in the page would confidently
// describe whichever machine it happens to be pointed at.
//
// Read-only, so it takes no config lock and changes nothing. A blank iface is
// a normal answer, not an error: see suggestRelayIface for when there is no
// single honest one to give.
func (s *Server) handleDHCPRelayIface(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Relays []string `json:"relays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"iface": suggestRelayIface(trimAll(req.Relays))})
}
