package webadmin

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// Everything else in this package checks the rendered config against Kea's
// grammar as documented. That is the check that passed for v944 while the
// server refused to start on every host it was deployed to, because the
// document was valid JSON, structurally what the manual describes, and
// rejected outright by the parser.
//
// So when a kea-dhcp4 binary is on the machine running the tests, use it. This
// skips where Kea is absent rather than failing, because it is a stronger
// check available on some hosts and not a new build dependency.

func keaBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("kea-dhcp4"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/kea-dhcp4", "/usr/local/sbin/kea-dhcp4"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	t.Skip("no kea-dhcp4 on this host; skipping the real-parser check")
	return ""
}

// realIface is an interface that exists here. Kea validates that every
// interface a subnet names is present on the system and refuses the whole file
// if one is not, so a fixture cannot use invented names.
func realIface(t *testing.T) string {
	t.Helper()
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces")
	}
	for _, i := range ifis {
		if i.Flags&net.FlagLoopback == 0 {
			return i.Name
		}
	}
	t.Skip("no non-loopback interface to name in a fixture")
	return ""
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kea-dhcp4.conf")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The whole point: a config gravinet renders is one Kea accepts.
func TestRealKeaAcceptsRenderedConfig(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{{
		Iface: iface, Subnet: "10.1.1.0/24",
		PoolStart: "10.1.1.10", PoolEnd: "10.1.1.245", Router: "10.1.1.1",
		DNS: []string{"10.1.1.1", "9.9.9.9"}, Search: []string{"lan.example", "corp.internal"},
		LeaseSeconds: 7200,
	}}}
	b, err := renderKea(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if why, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Errorf("Kea rejected a config gravinet rendered: %s\n%s", why, b)
	}
}

// The v944 shape, pinned against the real parser so the specific mistake
// cannot come back as a "Kea ignores unknown keys" assumption a second time.
func TestRealKeaRejectsMarkerBesideDhcp4(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{{
		Iface: iface, Subnet: "10.1.1.0/24", PoolStart: "10.1.1.10", PoolEnd: "10.1.1.245",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce v944: hoist the marker out of user-context to the top level.
	bad := strings.Replace(string(b), "{\n  \"Dhcp4\"", "{\n  \"gravinet-generated\": true,\n  \"Dhcp4\"", 1)
	why, ok := keaTestConf(writeTemp(t, []byte(bad)))
	if ok {
		t.Skip("this Kea tolerates a key beside Dhcp4; the one that reported the bug does not")
	}
	if !strings.Contains(why, "expecting Dhcp4") {
		t.Errorf("rejected for an unexpected reason, so this test is not pinning what it claims: %s", why)
	}
}

// keaTestConf has to report a failure as one, and say something useful about
// it. A checker that returns "ok" for a broken file is worse than no checker:
// the apply would go on to restart a unit that cannot start.
func TestKeaTestConfReportsTheReason(t *testing.T) {
	keaBin(t)
	why, ok := keaTestConf(writeTemp(t, []byte(`{"Dhcp4": {"subnet4": [{"subnet": "not-a-subnet"}]}}`)))
	if ok {
		t.Fatal("accepted a config with a malformed subnet")
	}
	if strings.TrimSpace(why) == "" {
		t.Error("rejected the config but reported no reason, which is the message this exists to provide")
	}
}

// Kea refuses the entire file for one interface it cannot find, so the apply
// drops those subnets first. Checked against the parser, because the
// consequence of getting it wrong is every other LAN on the node losing DHCP.
func TestRealKeaAcceptsConfigAfterDroppingAbsentIface(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{
		{Iface: iface, Subnet: "10.1.1.0/24", PoolStart: "10.1.1.10", PoolEnd: "10.1.1.245"},
		{Iface: "gravinet-no-such-nic0", Subnet: "192.168.50.0/24", PoolStart: "192.168.50.10", PoolEnd: "192.168.50.245"},
	}}

	// Unfiltered, this is the failure being defended against.
	if b, err := renderKea(c); err == nil {
		if _, ok := keaTestConf(writeTemp(t, b)); ok {
			t.Skip("this Kea does not validate interface presence; nothing to defend against here")
		}
	}

	served, dropped := servableSubnets(c)
	if len(dropped) != 1 || dropped[0] != "gravinet-no-such-nic0" {
		t.Fatalf("dropped = %v, want just the absent interface", dropped)
	}
	if got := len(served.EnabledSubnets()); got != 1 {
		t.Fatalf("kept %d subnets, want the one whose interface exists", got)
	}
	b, err := renderKea(served)
	if err != nil {
		t.Fatal(err)
	}
	if why, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Errorf("Kea still rejects the config after dropping the absent interface: %s", why)
	}
}

// A down interface is not an absent one. Kea starts on a down link, and
// dropping those would quietly stop serving a LAN whose cable was out for a
// minute.
func TestServableSubnetsKeepsDownInterfaces(t *testing.T) {
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces")
	}
	var down string
	for _, i := range ifis {
		if i.Flags&net.FlagUp == 0 && i.Flags&net.FlagLoopback == 0 {
			down = i.Name
			break
		}
	}
	if down == "" {
		t.Skip("no down interface on this host to check against")
	}
	c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{
		{Iface: down, Subnet: "10.9.9.0/24", PoolStart: "10.9.9.10", PoolEnd: "10.9.9.245"},
	}}
	served, dropped := servableSubnets(c)
	if len(dropped) != 0 {
		t.Errorf("dropped %v; a down interface still exists and Kea accepts it", dropped)
	}
	if len(served.EnabledSubnets()) != 1 {
		t.Error("the subnet on a down interface was removed")
	}
}

// --- relayed subnets (v969), against the real parser ---------------------

// The core of the feature, checked where it counts. A scope with a relay
// clause and no interface is the shape gravinet renders for a network this
// node is not attached to, and the only thing that makes it worth rendering
// is that Kea takes it.
func TestRealKeaAcceptsRelayedSubnet(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{{
		Iface: iface, Subnet: "10.9.1.0/24", Relays: []string{"10.9.1.1"},
		PoolStart: "10.9.1.100", PoolEnd: "10.9.1.200", Router: "10.9.1.1",
	}}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The subnet is nowhere near whatever this host's interface is really
	// addressed in, which is the whole point of a relayed scope.
	if why, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Errorf("Kea rejected a relayed scope: %s\n%s", why, b)
	}
}

// The shape the feature exists for: several branch LANs, all reached over
// one uplink, which one-subnet-per-interface used to make unrepresentable.
func TestRealKeaAcceptsManyRelayedSubnetsOnOneInterface(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	var subs []config.DHCPSubnet
	for _, n := range []string{"1", "2", "3"} {
		subs = append(subs, config.DHCPSubnet{
			Iface: iface, Subnet: "10.9." + n + ".0/24", Relays: []string{"10.9." + n + ".1"},
			PoolStart: "10.9." + n + ".100", PoolEnd: "10.9." + n + ".200",
		})
	}
	b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: subs})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if why, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Errorf("Kea rejected three relayed scopes sharing an interface: %s\n%s", why, b)
	}
}

// Why interfaces-config is deduplicated, pinned against the parser. Kea
// refuses the whole file for a repeated interface — not the repeat, the file
// — so without the dedup a node's second remote LAN behind an uplink it
// already served one behind would have stopped DHCP for every scope on it.
func TestRealKeaRefusesARepeatedListenInterface(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{
		{Iface: iface, Subnet: "10.9.1.0/24", Relays: []string{"10.9.1.1"}, PoolStart: "10.9.1.100", PoolEnd: "10.9.1.200"},
		{Iface: iface, Subnet: "10.9.2.0/24", Relays: []string{"10.9.2.1"}, PoolStart: "10.9.2.100", PoolEnd: "10.9.2.200"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// What gravinet renders is accepted...
	if why, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Fatalf("the deduplicated config was rejected: %s\n%s", why, b)
	}
	// ...and the undeduplicated version it would have rendered before v969
	// is the failure being defended against. Rebuilt through the parser
	// rather than by patching the text, so indentation cannot make this
	// silently stop testing anything.
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	ic := doc["Dhcp4"].(map[string]any)["interfaces-config"].(map[string]any)
	names := ic["interfaces"].([]any)
	if len(names) != 1 {
		t.Fatalf("interfaces = %v, want the shared uplink exactly once", names)
	}
	ic["interfaces"] = append(names, names[0])
	dup, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	why, ok := keaTestConf(writeTemp(t, dup))
	if ok {
		t.Skip("this Kea tolerates a repeated listen interface; 2.4.1 does not")
	}
	if !strings.Contains(why, "already been specified") {
		t.Errorf("rejected for an unexpected reason, so this test is not pinning what it claims: %s", why)
	}
}

// Kea accepts two subnets claiming one relay address and silently serves one
// of them, which is why gravinet refuses the pair on save rather than leaving
// it to the parser the way it leaves the duplicate interface. If a future Kea
// starts refusing it, this test says so and the config rule can be revisited.
func TestRealKeaDoesNotCatchAGiaddrCollision(t *testing.T) {
	keaBin(t)
	iface := realIface(t)
	c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{
		{Iface: iface, Subnet: "10.9.1.0/24", Relays: []string{"10.9.1.1"}, PoolStart: "10.9.1.100", PoolEnd: "10.9.1.200"},
		{Iface: iface, Subnet: "10.9.2.0/24", Relays: []string{"10.9.1.1"}, PoolStart: "10.9.2.100", PoolEnd: "10.9.2.200"},
	}}
	// gravinet's own rule catches it first, which is the point.
	if err := c.Validate(); err == nil {
		t.Fatal("gravinet accepted two subnets claiming one relay address")
	}
	b, err := renderKea(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keaTestConf(writeTemp(t, b)); !ok {
		t.Log("this Kea now refuses a giaddr collision too; gravinet's rule is no longer the only guard")
	}
}
