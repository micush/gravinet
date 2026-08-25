package webadmin

import (
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
