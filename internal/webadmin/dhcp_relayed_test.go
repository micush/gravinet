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
