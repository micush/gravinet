package webadmin

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// The socket type is what decides whether a relayed scope is servable at all,
// and it is not visible anywhere on the page, so it is pinned here.
//
// Kea's raw sockets watch a named interface's wire. A relayed request is a
// unicast to this host's address that arrives over whatever path reaches the
// relay, which is almost never that wire, so under raw it is never seen. Only
// udp binds the address itself and is delivered it regardless of arrival
// interface. Checked against kea-dhcp4 2.4.1: the same relayed DISCOVER
// produced DHCP4_SUBNET_SELECTED and DHCP4_LEASE_ADVERT under udp and no log
// line at all under raw.
func TestKeaSocketTypeFollowsTheScopes(t *testing.T) {
	attached := config.DHCPSubnet{
		Iface: "eth1", Subnet: "10.1.1.0/24",
		PoolStart: "10.1.1.10", PoolEnd: "10.1.1.245", Router: "10.1.1.1",
	}
	relayed := config.DHCPSubnet{
		Iface: "eth1", Subnet: "10.4.4.0/24", Relays: []string{"10.4.4.1"},
		PoolStart: "10.4.4.10", PoolEnd: "10.4.4.245", Router: "10.4.4.1",
	}

	for name, tc := range map[string]struct {
		subnets []config.DHCPSubnet
		want    string
	}{
		"attached only":          {[]config.DHCPSubnet{attached}, "raw"},
		"relayed only":           {[]config.DHCPSubnet{relayed}, "udp"},
		"both, relayed decides":  {[]config.DHCPSubnet{attached, relayed}, "udp"},
		"both, order irrelevant": {[]config.DHCPSubnet{relayed, attached}, "udp"},
	} {
		b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: tc.subnets})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got struct {
			Dhcp4 struct {
				InterfacesConfig struct {
					SocketType string `json:"dhcp-socket-type"`
				} `json:"interfaces-config"`
			} `json:"Dhcp4"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Dhcp4.InterfacesConfig.SocketType != tc.want {
			t.Errorf("%s: dhcp-socket-type = %q, want %q",
				name, got.Dhcp4.InterfacesConfig.SocketType, tc.want)
		}
	}
}

// A node with both kinds of scope loses its attached clients, because Kea's
// socket type is global and renderKea resolves the conflict towards the
// relayed ones. That is the right resolution and it must not be silent.
func TestMixedScopesAreReported(t *testing.T) {
	attached := config.DHCPSubnet{
		Iface: "eth1", Subnet: "10.1.1.0/24",
		PoolStart: "10.1.1.10", PoolEnd: "10.1.1.245",
	}
	relayed := config.DHCPSubnet{
		Iface: "eth1", Subnet: "10.4.4.0/24", Relays: []string{"10.4.4.1"},
		PoolStart: "10.4.4.10", PoolEnd: "10.4.4.245",
	}

	mixed := config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{attached, relayed}}
	w := dhcpMixedScopeWarning(mixed)
	if w == "" {
		t.Fatal("a node serving both attached and relayed scopes was not warned about")
	}
	if !strings.Contains(w, "eth1") {
		t.Errorf("the warning does not name the link that stops being served: %q", w)
	}
	// And it reaches the note the apply actually shows.
	if note := dhcpProblemNote(mixed); !strings.Contains(note, "attached") {
		t.Errorf("the warning did not reach the apply note: %q", note)
	}

	// Neither kind alone is a problem.
	for name, c := range map[string]config.DHCPConfig{
		"attached only": {Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{attached}},
		"relayed only":  {Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{relayed}},
		"relay mode":    {Mode: config.DHCPRelay, Subnets: []config.DHCPSubnet{attached, relayed}},
	} {
		if got := dhcpMixedScopeWarning(c); got != "" {
			t.Errorf("%s: warned unnecessarily: %q", name, got)
		}
	}
}

// The end-to-end check the relayed-scope work was missing: render a relayed
// scope, start the real kea-dhcp4 on it, and put a forwarded DISCOVER through
// it from an interface Kea is not listening on. That last part is the whole
// point — it is what a relay across an overlay, a tunnel or an uplink looks
// like from the server's side, and it is what the rendered socket type decides.
//
// Skipped without kea-dhcp4, and without the privilege to hold port 67.
func TestRealKeaAnswersARelayedDiscover(t *testing.T) {
	bin := keaBin(t)
	iface, self := addressedIface(t)

	b, err := renderKea(config.DHCPConfig{
		Mode: config.DHCPServer,
		Subnets: []config.DHCPSubnet{{
			Iface: iface, Subnet: "10.4.4.0/24", Relays: []string{"10.4.4.1"},
			PoolStart: "10.4.4.10", PoolEnd: "10.4.4.245", Router: "10.4.4.1",
		}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	dir := t.TempDir()
	confPath := filepath.Join(dir, "kea-dhcp4.conf")
	logPath := filepath.Join(dir, "kea.log")
	// Point the logger at a file this test can read, and give the lease file
	// somewhere writable, without disturbing what renderKea produced above.
	b = []byte(strings.Replace(string(b), `"output": "syslog"`,
		`"output": "`+logPath+`"`, 1))
	b = []byte(strings.Replace(string(b), keaLeasePath, filepath.Join(dir, "leases.csv"), 1))
	if err := os.WriteFile(confPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-c", confPath)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start kea-dhcp4: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := os.ReadFile(logPath); err == nil && strings.Contains(string(s), "DHCP4_STARTED") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s, _ := os.ReadFile(logPath); !strings.Contains(string(s), "DHCP4_STARTED") {
		t.Skip("kea-dhcp4 did not start here (port 67 likely unavailable)")
	}
	if s, _ := os.ReadFile(logPath); strings.Contains(string(s), "OPEN_SOCKET_FAIL") {
		t.Fatalf("kea started but could not open its socket:\n%s", s)
	}

	// A DISCOVER as a relay would forward it: hops set, giaddr stamped with
	// the far relay's address, sent as a unicast to this host. It leaves from
	// loopback, so it arrives on an interface Kea was never told about.
	if err := sendRelayedDiscover("10.4.4.1", self); err != nil {
		t.Skipf("cannot send a raw packet here: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	log, _ := os.ReadFile(logPath)
	// DHCP4_LEASE_ADVERT rather than DHCP4_SUBNET_SELECTED, which says more
	// but is logged at DEBUG and renderKea configures INFO. Asserting on a
	// line the rendered config cannot produce would be a test that only ever
	// passed against a hand-edited file.
	if !strings.Contains(string(log), "DHCP4_LEASE_ADVERT") {
		t.Errorf("the relayed request was not served:\n%s", log)
	}
	// And the lease came from the relayed scope, chosen by giaddr — not from
	// some default that would have answered any request at all.
	if !strings.Contains(string(log), "10.4.4.") {
		t.Errorf("a lease was advertised but not from the relayed scope:\n%s", log)
	}
	// The socket type is what made it reachable. Pinned here too, because
	// this test passing under "raw" would mean the packet took a path this
	// test did not intend.
	if !strings.Contains(string(log), "using socket type udp") {
		t.Errorf("kea did not select the udp socket type:\n%s", log)
	}
}

// addressedIface returns an interface that is up and carries a usable IPv4
// address, with that address. Not realIface, which takes the first
// non-loopback NIC and so can land on a dummy or an ifb with no address —
// fine for a fixture that only needs a name Kea will accept, useless here,
// where the address is what the server binds and what the request is aimed at.
func addressedIface(t *testing.T) (string, string) {
	t.Helper()
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skip("cannot enumerate interfaces")
	}
	for _, i := range ifis {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, p := range v4Prefixes(addrs) {
			return i.Name, p.Addr().String()
		}
	}
	t.Skip("no up, addressed, non-loopback interface to serve from")
	return "", ""
}
