package webadmin

import (
	"net"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// Serving networks this node is not attached to (v969).
//
// The mechanism is Kea's: a relay agent on a distant segment forwards its
// clients' broadcasts here as unicast under its own address, and Kea picks the
// scope by matching that address rather than by the receiving interface's. So
// what these check is that gravinet renders a scope Kea can actually select
// that way, and does not apply the attached-subnet rules to it.

func relayedKeaSubnet(prefix, giaddr string) config.DHCPSubnet {
	return config.DHCPSubnet{
		Iface: "eth0", Subnet: prefix + ".0/24", Relays: []string{giaddr},
		PoolStart: prefix + ".100", PoolEnd: prefix + ".200", Router: prefix + ".1",
	}
}

func keaSubnets(t *testing.T, c config.DHCPConfig) ([]map[string]any, []string) {
	t.Helper()
	m := renderKeaMap(t, c)
	d, _ := m["Dhcp4"].(map[string]any)
	if d == nil {
		t.Fatal("no Dhcp4 object")
	}
	var subs []map[string]any
	for _, s := range d["subnet4"].([]any) {
		subs = append(subs, s.(map[string]any))
	}
	var ifaces []string
	ic, _ := d["interfaces-config"].(map[string]any)
	for _, n := range ic["interfaces"].([]any) {
		ifaces = append(ifaces, n.(string))
	}
	return subs, ifaces
}

// The scope Kea needs, and the key that must not be in it. "interface"
// asserts the subnet is on that link and Kea validates the claim — a remote
// subnet naming one is the whole file refused, not the one scope.
func TestRenderKeaRelayedSubnetIsSelectedByGiaddrNotInterface(t *testing.T) {
	subs, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode:    config.DHCPServer,
		Subnets: []config.DHCPSubnet{relayedKeaSubnet("10.9.1", "10.9.1.1")},
	})
	if len(subs) != 1 {
		t.Fatalf("want 1 subnet, got %d", len(subs))
	}
	s := subs[0]
	if _, named := s["interface"]; named {
		t.Errorf("a relayed scope names an interface (%v) — Kea rejects the entire configuration for that", s["interface"])
	}
	rel, _ := s["relay"].(map[string]any)
	if rel == nil {
		t.Fatal("a relayed scope rendered no relay clause, so no forwarded request could ever select it")
	}
	got, _ := rel["ip-addresses"].([]any)
	if len(got) != 1 || got[0] != "10.9.1.1" {
		t.Errorf("relay ip-addresses = %v, want the giaddr the agent forwards under", got)
	}
	// The interface is still listened on: the scope is not on it, but the
	// unicast from the relay arrives there and Kea only binds where told.
	if len(ifaces) != 1 || ifaces[0] != "eth0" {
		t.Errorf("interfaces-config = %v, want the link the relay reaches this host over", ifaces)
	}
}

// An attached scope is unchanged: named interface, no relay clause. An empty
// relay clause is not the same thing to Kea as none — it is a scope reachable
// by no relay and no interface either.
func TestRenderKeaAttachedSubnetIsUnchanged(t *testing.T) {
	subs, _ := keaSubnets(t, config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
	if subs[0]["interface"] != "eth1" {
		t.Errorf("interface = %v, want the link the clients are on", subs[0]["interface"])
	}
	if _, has := subs[0]["relay"]; has {
		t.Error("an attached scope rendered a relay clause")
	}
}

// The shape this feature exists for: several branch LANs behind relays, all
// arriving over one uplink. The interface is named once — Kea takes a
// repeated interface as a second request to open a socket already open.
func TestRenderKeaManyRelayedSubnetsNameTheirSharedLinkOnce(t *testing.T) {
	subs, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode: config.DHCPServer,
		Subnets: []config.DHCPSubnet{
			relayedKeaSubnet("10.9.1", "10.9.1.1"),
			relayedKeaSubnet("10.9.2", "10.9.2.1"),
			relayedKeaSubnet("10.9.3", "10.9.3.1"),
		},
	})
	if len(subs) != 3 {
		t.Fatalf("want 3 scopes, got %d", len(subs))
	}
	if len(ifaces) != 1 || ifaces[0] != "eth0" {
		t.Errorf("interfaces-config = %v, want the shared uplink exactly once", ifaces)
	}
	// Ids stay distinct, or Kea refuses the file and leases would not stay
	// attached to the scope they were issued from.
	seen := map[float64]bool{}
	for _, s := range subs {
		id := s["id"].(float64)
		if id < 1 || seen[id] {
			t.Errorf("subnet id %v is reserved or repeated", id)
		}
		seen[id] = true
	}
}

// A node serving its own LAN and remote ones at once renders both kinds side
// by side, each selected its own way.
func TestRenderKeaMixesAttachedAndRelayedScopes(t *testing.T) {
	local := dhcpSubnet()
	local.Iface = "eth0"
	subs, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode:    config.DHCPServer,
		Subnets: []config.DHCPSubnet{local, relayedKeaSubnet("10.9.1", "10.9.1.1")},
	})
	if len(subs) != 2 {
		t.Fatalf("want 2 scopes, got %d", len(subs))
	}
	if subs[0]["interface"] != "eth0" {
		t.Errorf("the attached scope lost its interface: %v", subs[0])
	}
	if _, named := subs[1]["interface"]; named {
		t.Errorf("the relayed scope gained one: %v", subs[1])
	}
	if len(ifaces) != 1 {
		t.Errorf("interfaces-config = %v, want the one shared link", ifaces)
	}
}

// The preflight condition a relayed subnet is *supposed* to fail. Running the
// attached check against one would put a red row under every correctly
// configured remote network on the node.
func TestDHCPPreflightDoesNotHoldRelayedSubnetsToTheAttachedRule(t *testing.T) {
	// A real interface with a usable IPv4 address, because that is the
	// situation being distinguished: the interface is fine, and the only
	// thing "wrong" with the row is that its subnet is somewhere else.
	// Loopback will not do — v4Prefixes drops it as unusable for DHCP, which
	// is a different fault and would pass this test for the wrong reason.
	name := ""
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces here")
	}
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aerr := ifi.Addrs()
		if aerr != nil || len(v4Prefixes(addrs)) == 0 {
			continue
		}
		name = ifi.Name
		break
	}
	if name == "" {
		t.Skip("no up interface with a usable IPv4 address on this host")
	}
	// TEST-NET-2, so it cannot collide with whatever the host is really
	// addressed in and accidentally satisfy the attached check.
	s := relayedKeaSubnet("198.51.100", "198.51.100.1")
	s.Iface = name
	if p := dhcpServerProblem(s, name); p != "" {
		t.Errorf("a relayed subnet was reported broken for not being on its interface: %q", p)
	}
	attached := s
	attached.Relays = nil
	if p := dhcpServerProblem(attached, name); p == "" {
		t.Error("the attached check stopped firing — an interface addressed outside the subnet it serves never answers")
	}
}

// The other half: a relayed subnet is still held to the condition that does
// apply to it. The relay forwards to an address on this host, so an interface
// without one has nowhere for those requests to arrive.
func TestDHCPPreflightStillWantsAnAddressForRelayedTraffic(t *testing.T) {
	// An up interface carrying no address DHCP could use. Loopback is the
	// usual one — v4Prefixes drops 127.0.0.1 as unusable — but a host can
	// have a global address on lo, so this looks rather than assumes and
	// skips rather than failing on somebody else's network setup.
	name := ""
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces here")
	}
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aerr := ifi.Addrs()
		if aerr != nil || len(v4Prefixes(addrs)) > 0 {
			continue
		}
		name = ifi.Name
		break
	}
	if name == "" {
		t.Skip("every up interface here has a usable IPv4 address")
	}
	s := relayedKeaSubnet("10.9.1", "10.9.1.1")
	s.Iface = name
	p := dhcpServerProblem(s, name)
	if p == "" || !strings.Contains(p, "nowhere to arrive") {
		t.Errorf("an interface with no usable IPv4 was accepted for relayed traffic: %q", p)
	}
}

// One reason per interface, and which one must not depend on the order rows
// happen to be stored in now that several subnets legitimately share a link.
func TestDHCPProblemsAreStableWhenSubnetsShareAnInterface(t *testing.T) {
	mk := func(order ...config.DHCPSubnet) string {
		return dhcpProblems(config.DHCPConfig{Mode: config.DHCPServer, Subnets: order})["definitely-not-a-nic"]
	}
	a := relayedKeaSubnet("10.9.1", "10.9.1.1")
	a.Iface = "definitely-not-a-nic"
	b := relayedKeaSubnet("10.9.2", "10.9.2.1")
	b.Iface = "definitely-not-a-nic"
	first, second := mk(a, b), mk(b, a)
	if first == "" || second == "" {
		t.Fatalf("a missing interface went unreported: %q / %q", first, second)
	}
	if !strings.Contains(first, "no interface by that name") {
		t.Errorf("unexpected reason: %q", first)
	}
}

// The refusal an operator hits while adding the second remote LAN. It is
// allowed now, and where it still applies it has to say what to do instead —
// the v968 wording named the restriction and not the way past it.
func TestDHCPDuplicateInterfaceRefusalPointsAtTheRelayColumn(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	add := between(t, src, `if req.Op == "add" {`, `d.Subnets = append(d.Subnets, e)`)
	if !strings.Contains(add, "!e.Relayed() && !x.Relayed()") {
		t.Error("the duplicate-interface refusal is not scoped to attached subnets, so a second relayed subnet is still refused")
	}
	if !strings.Contains(add, "relay address") {
		t.Error("the refusal does not mention the relay column, which is the thing the operator needed to know")
	}
}

// The handler has to carry the field the editor posts. Joined only by the
// JSON name, which is the seam that goes quietly wrong: a relay address typed
// into the page and dropped here produces an attached scope for a subnet that
// is nowhere near the interface, and Kea refuses the whole file.
func TestDHCPHandlerCarriesRelaysThrough(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	if !strings.Contains(src, `Relays       []string `+"`"+`json:"relays"`+"`") {
		t.Error("the POST body has no relays field")
	}
	if !strings.Contains(src, "Relays: trimAll(req.Relays)") {
		t.Error("relays are decoded but never stored on the subnet")
	}
}

// --- the editor ---------------------------------------------------------
//
// Structural, on the rendered JS source, the way every other UI check in this
// package is. What they pin is that the column exists, round-trips, and does
// not drag the attached-subnet prefill along with it.

func TestDHCPServerTableHasARelayColumn(t *testing.T) {
	if !strings.Contains(indexHTML, "<th>interface</th><th>relay</th><th>subnet</th>") {
		t.Error("the server table has no relay column, so a relayed subnet cannot be told from an attached one at a glance")
	}
	// Adding a column without moving the colspans leaves the red problem row
	// and the empty state short, which reads as a broken table.
	for _, want := range []string{`colspan="9" class="empty"`, `colspan="9" class="err"`} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("missing %s — a colspan was left at the old column count", want)
		}
	}
}

func TestDHCPRelayColumnRoundTrips(t *testing.T) {
	for _, want := range []string{
		`class="dhe-relays"`,         // the input exists
		"data-relays=",               // the rendered row carries it
		"relays: csv('.dhe-relays')", // it is read back on save
		"relays:tr.dataset.relays",   // and reloaded when editing in place
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("the relay column does not round-trip: missing %q", want)
		}
	}
}

// Choosing an interface fills an attached row in from that interface's own
// address. On a relayed row that address describes the wrong network
// entirely, and prefilling would overwrite a remote subnet with the uplink's.
func TestDHCPPrefillLeavesRelayedRowsAlone(t *testing.T) {
	body := between(t, indexHTML, "function dhPrefill(tr, name){", "\n  }")
	if !strings.Contains(body, ".dhe-relays") {
		t.Error("the prefill does not consult the relay column, so picking an interface overwrites a remote subnet with the uplink's own")
	}
	guard := between(t, body, ".dhe-relays", "const next")
	if !strings.Contains(guard, "return") {
		t.Error("the prefill reads the relay column but does not bail out on it")
	}
}

// The help text is where the mechanism is explained, and an operator looking
// for "how do I serve a network I am not on" has to be able to find it there.
func TestDHCPHelpExplainsServingANetworkThisNodeIsNotOn(t *testing.T) {
	topic := between(t, indexHTML, "'dhcp': {", "'snmp': {")
	for _, want := range []string{"relay agent", "ip helper-address", "giaddr"} {
		if !strings.Contains(topic, want) {
			t.Errorf("the DHCP help never mentions %q, so the feature is undiscoverable from the page", want)
		}
	}
	if !strings.Contains(topic, "'relay':") {
		t.Error("the relay column has no column note, unlike every other column on the page")
	}
}

// The reply path (v978/v979).
//
// v972 made a relayed DISCOVER *arrive*: udp sockets match on address, so a
// unicast to eth1's address coming over the mesh reaches Kea. Its notes then
// claimed the overlay device never has to be named, which was true of
// receiving and untrue of answering.
//
// Kea routes the reply to giaddr, which is on the far side of the relay, so it
// egresses the link that reaches the relay. It then requires one of its own
// sockets on that link and drops the packet when there is none:
//
//	DHCP4_PACKET_SEND_FAIL ... failed to send DHCPv4 packet:
//	    Interface mesh0/19 does not have any suitable IPv4 sockets open.
//
// The iface column on a relayed row is that link, and it is already what
// renderKea puts in interfaces-config. What was missing was being allowed to
// put a mesh device there.
func TestRenderKeaListensOnTheRelayReplyInterface(t *testing.T) {
	sub := relayedKeaSubnet("10.4.4", "10.4.4.1")
	sub.Iface = "mesh0" // the link this node answers the relay across
	subs, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub},
	})
	if len(ifaces) != 1 || ifaces[0] != "mesh0" {
		t.Errorf("interfaces = %v, want [mesh0] %s Kea cannot send a relayed reply "+
			"without a socket on the link that reaches the relay", ifaces, EMDASH)
	}
	// And still selected by giaddr alone: naming the reply link must not put
	// an "interface" key on the scope, which would assert the remote subnet is
	// on the overlay and have Kea refuse the file.
	if _, ok := subs[0]["interface"]; ok {
		t.Error("the relayed scope gained an interface key; it must be selected by giaddr only")
	}
}

// A mesh device is refusable on an attached row and not on a relayed one. The
// rule exists to keep a DHCP server off the overlay, and a relayed scope
// cannot be one: it carries no interface key, so it is selected by giaddr, and
// no giaddr is an overlay address.
func TestRelayedRowMayNameAMeshInterface(t *testing.T) {
	src := readSource(t, "dhcp_apply.go")
	if !strings.Contains(src, "if !e.Relayed() {") {
		t.Error("the mesh-interface refusal is unconditional again; a relayed row cannot name its reply link")
	}
	if !strings.Contains(src, "s.refuseMeshIface(e.Iface)") {
		t.Error("the mesh-interface refusal is gone entirely; an attached subnet could be served on the overlay")
	}
}

// The suggestion offered for a relayed row's iface column.
func TestSuggestRelayIfaceIsBlankWhenRelaysDisagree(t *testing.T) {
	// Loopback resolves; TEST-NET-1 almost certainly does not. Either they
	// differ or the second is unresolvable, and both must yield no guess: one
	// column cannot answer two links, and picking one would silently drop the
	// other relay's clients.
	if got := suggestRelayIface([]string{"127.0.0.1", "192.0.2.77"}); got != "" {
		t.Errorf("suggestRelayIface for two unlike relays = %q, want no guess", got)
	}
	if got := suggestRelayIface([]string{"127.0.0.1"}); got == "" {
		t.Skip("no route to 127.0.0.1 on this host")
	}
}

// The warning that replaces the silent failure: a relayed row whose iface
// column is not the link its relay is reached over is named in the apply note.
func TestRelayIfaceNoteNamesAMismatchedRow(t *testing.T) {
	lo, ok := egressIfaceFor("127.0.0.1")
	if !ok {
		t.Skip("no route to 127.0.0.1 on this host")
	}
	sub := relayedKeaSubnet("10.4.4", "127.0.0.1")
	sub.Iface = "definitely-not-" + lo
	note := relayIfaceNote(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub}})
	if !strings.Contains(note, sub.Iface) || !strings.Contains(note, lo) {
		t.Errorf("note = %q, want it to name both the column's value and the real egress", note)
	}

	sub.Iface = lo
	if note := relayIfaceNote(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub}}); note != "" {
		t.Errorf("a correctly-pointed row still warns: %q", note)
	}
}

// An attached-only config has no relay and nothing to say about one.
func TestRelayIfaceNoteIsSilentForAttachedOnly(t *testing.T) {
	c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}}
	if note := relayIfaceNote(c); note != "" {
		t.Errorf("attached-only config warned about relays: %q", note)
	}
}

// The route lookup sends nothing and must agree with the host's own view.
func TestEgressIfaceForResolvesALocalAddress(t *testing.T) {
	name, ok := egressIfaceFor("127.0.0.1")
	if !ok {
		t.Skip("no route to 127.0.0.1 on this host")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatalf("egressIfaceFor returned %q, which is not an interface: %v", name, err)
	}
	if iface.Flags&net.FlagLoopback == 0 {
		t.Errorf("egressIfaceFor(127.0.0.1) = %q, which is not a loopback interface", name)
	}
}

const EMDASH = "\u2014"

// A tagged interface is an ordinary device name to everything here, and the
// dot in it is the sort of thing a later "tidy the interface name" change
// splits on. Nothing downstream may touch it: Kea takes the device name
// verbatim, and a truncated one names a link this host does not have.
//
// A VLAN cannot ride on a mesh device — refuseVLANParent stops that at
// creation — so the reply link for a relayed row is a tagged LAN interface or
// a mesh device, never a tagged mesh device.
func TestRenderKeaKeepsATaggedReplyInterfaceIntact(t *testing.T) {
	sub := relayedKeaSubnet("10.4.4", "10.4.4.1")
	sub.Iface = "eth1.22"
	_, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub},
	})
	if len(ifaces) != 1 || ifaces[0] != "eth1.22" {
		t.Errorf("interfaces = %v, want [eth1.22] verbatim", ifaces)
	}
}

// And the same for an attached row, where the tagged interface is also the
// scope's own link and so appears twice in the file — once as the subnet's
// "interface" and once in interfaces-config. Both must be the whole name.
func TestRenderKeaKeepsATaggedAttachedInterfaceIntact(t *testing.T) {
	sub := dhcpSubnet()
	sub.Iface = "eth1.22"
	subs, ifaces := keaSubnets(t, config.DHCPConfig{
		Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub},
	})
	if len(ifaces) != 1 || ifaces[0] != "eth1.22" {
		t.Errorf("interfaces = %v, want [eth1.22] verbatim", ifaces)
	}
	if got, _ := subs[0]["interface"].(string); got != "eth1.22" {
		t.Errorf("subnet interface = %q, want eth1.22 verbatim", got)
	}
}

// The mismatch warning names the tagged interface as the operator wrote it,
// rather than a stem of it, or the note points at a device they cannot find.
func TestRelayIfaceNoteKeepsATaggedNameIntact(t *testing.T) {
	lo, ok := egressIfaceFor("127.0.0.1")
	if !ok {
		t.Skip("no route to 127.0.0.1 on this host")
	}
	sub := relayedKeaSubnet("10.4.4", "127.0.0.1")
	sub.Iface = "eth1.22"
	if lo == sub.Iface {
		t.Skip("loopback is named eth1.22 on this host, which defeats the test")
	}
	note := relayIfaceNote(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{sub}})
	if !strings.Contains(note, "eth1.22") {
		t.Errorf("note = %q, want it to name eth1.22 in full", note)
	}
}

// The picker offers overlay devices on the server table and not on the
// relay-links table.
//
// v979 shipped this gated on the row already carrying a relay address, which
// cannot work: the interface column is left of the relay column, so on a new
// row the interface is chosen while the row is still attached and the overlay
// devices are not in the list yet. Reported as "I still cannot choose mesh0".
//
// The gate is now the table rather than the row's half-typed state. Pinned as
// the call sites, because the failure is silent — the select simply renders
// one option short and nothing anywhere says why.
func TestDHCPServerPickerOffersOverlayDevices(t *testing.T) {
	if !strings.Contains(indexHTML, `'<td><select class="dhe-iface" style="width:100px">'+dhIfaceOpts(e.iface||'', true)`) {
		t.Error("the server table's interface picker no longer offers overlay devices; a relayed row cannot name its reply link")
	}
	if !strings.Contains(indexHTML, `'<td><select class="dle-iface" style="width:100px">'+dhIfaceOpts(e.iface||'')`) {
		t.Error("the relay-links picker gained overlay devices; a relay link must be a LAN interface")
	}
	if strings.Contains(indexHTML, "dhIfaceOpts(e.iface||'', !!(e.relays") {
		t.Error("the picker depends on the row already being relayed again, which no order of entry satisfies")
	}
}

// meshIfaceNames must be declared in the DHCP section that assigns it. It was
// first added to the IPv6 RA section instead, where secDHCP cannot see it, so
// the assignment fell through to an implicit global — which happens to work
// and would stop working under strict mode or a bundler.
func TestDHCPSectionOwnsItsMeshInterfaceList(t *testing.T) {
	i := strings.Index(indexHTML, "function secDHCP(")
	if i < 0 {
		t.Fatal("secDHCP not found")
	}
	body := indexHTML[i:]
	if j := strings.Index(body, "\nfunction "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "meshIfaceNames = []") {
		t.Error("secDHCP assigns meshIfaceNames without declaring it; the declaration is in another section's scope")
	}
}

// A newly created interface has to reach the pickers.
//
// systemInterfaces() caches /api/interfaces, and that cache was cleared in one
// place only: setTarget, on a managed-node switch. Creating eth1.22 under
// System > VLANs and then opening DHCP therefore built the picker from a list
// fetched before the VLAN existed, and the interface was simply absent with
// nothing to explain it.
//
// Pinned at the three points that must drop it: rail navigation, search
// navigation, and a VLAN add or delete. The last one matters on its own,
// because it is the case where the operator never leaves the page.
func TestInterfaceCacheIsDroppedWhenTheHostChanges(t *testing.T) {
	if !strings.Contains(indexHTML, "function forgetInterfaces(){ _ifaceCache = null; }") {
		t.Fatal("forgetInterfaces is gone; nothing drops the interface list short of a page reload")
	}
	for _, site := range []struct{ what, src string }{
		{"rail navigation", "setActiveRailTab(s); forgetInterfaces(); refresh();"},
		{"search navigation", "state.section = targetSection;\n  forgetInterfaces();"},
		{"a VLAN add or delete", "forgetInterfaces();\n      renderSection();"},
	} {
		if !strings.Contains(indexHTML, site.src) {
			t.Errorf("%s no longer drops the cached interface list; a new interface will not appear in any picker", site.what)
		}
	}
	// Deliberately not on every renderSection: that runs on the periodic
	// status refresh too, and refetching the interface list every few seconds
	// is the wrong trade for a change that happens rarely.
	if strings.Contains(indexHTML, "function renderSection() {\n  forgetInterfaces();") {
		t.Error("forgetInterfaces moved into renderSection; /api/interfaces is now on the status poll")
	}
}
