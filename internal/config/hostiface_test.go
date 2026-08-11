package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gravinet/internal/hostnet"
)

// The point of the field: addressing travels with the configuration, so a
// backup taken on one day and restored on another brings the node's own IP
// addresses back with everything else.
func TestHostIfaceRoundTripsThroughConfig(t *testing.T) {
	c := &Config{UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.SetHostIface(HostIface{
		Iface: "eth1", Addrs: []string{"10.1.1.5/24", "fd00::5/64"}, GW4: "10.1.1.1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("config with host interfaces should validate: %v", err)
	}

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	h := back.HostIfaceFor("eth1")
	if h == nil {
		t.Fatal("the interface did not survive a config round trip")
	}
	if len(h.Addrs) != 2 || h.Addrs[0] != "10.1.1.5/24" || h.GW4 != "10.1.1.1" {
		t.Fatalf("addressing came back wrong: %+v", h)
	}

	// A config with no managed interfaces must serialize exactly as before,
	// so existing configs round-trip unchanged.
	plain := &Config{UDPPorts: []int{51820}, EnableIPv4: true}
	pb, _ := json.Marshal(plain)
	if strings.Contains(string(pb), "host_interfaces") {
		t.Error("host_interfaces should be omitted when empty")
	}
}

// Editing the addresses must not silently drop the default route, and vice
// versa — the two edits are separate in the UI and must not undo each other.
func TestSetHostIfaceKeepsGatewayWhenNotGiven(t *testing.T) {
	c := &Config{}
	if err := c.SetHostIface(HostIface{Iface: "eth1", Addrs: []string{"10.1.1.5/24"}, GW4: "10.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	// An addresses-only update carries no gateway.
	if err := c.SetHostIface(HostIface{Iface: "eth1", Addrs: []string{"10.1.1.6/24"}}); err != nil {
		t.Fatal(err)
	}
	h := c.HostIfaceFor("eth1")
	if h.GW4 != "10.1.1.1" {
		t.Errorf("the gateway was dropped by an addresses-only edit: %+v", h)
	}
	if h.Addrs[0] != "10.1.1.6/24" {
		t.Errorf("the address was not updated: %+v", h)
	}
	if len(c.HostInterfaces) != 1 {
		t.Errorf("updating should replace, not append: %+v", c.HostInterfaces)
	}
}

func TestHostIfaceValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   HostIface
		ok   bool
	}{
		{"ok", HostIface{Iface: "eth1", Addrs: []string{"10.1.1.5/24"}, GW4: "10.1.1.1"}, true},
		{"no iface", HostIface{Addrs: []string{"10.1.1.5/24"}}, false},
		{"bare address", HostIface{Iface: "eth1", Addrs: []string{"10.1.1.5"}}, false},
		// The kernel owns these; a config claiming them would fight it on
		// every reconcile.
		{"link-local", HostIface{Iface: "eth1", Addrs: []string{"fe80::1/64"}}, false},
		{"loopback", HostIface{Iface: "eth1", Addrs: []string{"127.0.0.1/8"}}, false},
		{"v6 in gw4", HostIface{Iface: "eth1", GW4: "fd00::1"}, false},
		{"v4 in gw6", HostIface{Iface: "eth1", GW6: "10.1.1.1"}, false},
		{"no addresses is allowed", HostIface{Iface: "eth1"}, true},
	} {
		err := tc.in.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// An untouched config has to serialize byte for byte as it did, or every host on
// the fleet gains a diff on the first reload after upgrading. The modes are
// omitempty for that reason, and this is the assertion that keeps them so.
func TestUnsetModesAreNotEncoded(t *testing.T) {
	c := &Config{UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.SetHostIface(HostIface{Iface: "eth1", Addrs: []string{"10.1.1.5/24"}}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode4", "mode6"} {
		if strings.Contains(string(b), key) {
			t.Errorf("an unset mode was encoded as %q: %s", key, b)
		}
	}

	// A record with no mode field is exactly what every pre-modes config looks
	// like, and it has to come back meaning static — those records exist only
	// because an operator set a static address.
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	h := back.HostIfaceFor("eth1")
	if h == nil {
		t.Fatal("the record did not survive the round trip")
	}
	if !h.Mode4.IsStatic() || !h.Mode6.IsStatic() {
		t.Errorf("a record with no modes must read as static: %+v", h)
	}

	// A mode that was set does travel.
	if err := c.SetHostIface(HostIface{Iface: "eth2", Mode4: hostnet.ModeDHCP, Mode6: hostnet.ModeSLAAC}); err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mode4":"dhcp"`, `"mode6":"slaac"`} {
		if !strings.Contains(string(b2), want) {
			t.Errorf("missing %s in %s", want, b2)
		}
	}
}

// A static address or gateway under a non-static mode would never be applied by
// anything: the address comes from a lease or an advertisement, and a gateway
// configured alongside would compete with the default route that arrives with
// it. Refused rather than dropped in transit — silently discarding something an
// operator typed is how a page comes to disagree with the interface it claims to
// describe.
func TestModesRefuseWhatCannotApply(t *testing.T) {
	for _, c := range []struct {
		name string
		h    HostIface
		says string
	}{
		{"v4 address under dhcp",
			HostIface{Iface: "eth1", Mode4: hostnet.ModeDHCP, Addrs: []string{"10.1.1.5/24"}},
			"IPv4"},
		{"v6 address under slaac",
			HostIface{Iface: "eth1", Mode6: hostnet.ModeSLAAC, Addrs: []string{"fd00::5/64"}},
			"IPv6"},
		{"v6 address under dhcp6",
			HostIface{Iface: "eth1", Mode6: hostnet.ModeDHCP6, Addrs: []string{"fd00::5/64"}},
			"IPv6"},
		{"v4 gateway under dhcp",
			HostIface{Iface: "eth1", Mode4: hostnet.ModeDHCP, GW4: "10.1.1.1"},
			"gw4"},
		{"v6 gateway under slaac",
			HostIface{Iface: "eth1", Mode6: hostnet.ModeSLAAC, GW6: "fd00::1"},
			"gw6"},
		{"an IPv6 mode in the IPv4 field",
			HostIface{Iface: "eth1", Mode4: hostnet.ModeSLAAC},
			"IPv6"},
		{"an IPv4 mode in the IPv6 field",
			HostIface{Iface: "eth1", Mode6: hostnet.ModeDHCP},
			"dhcp6"},
	} {
		err := c.h.Validate()
		if err == nil {
			t.Errorf("%s: should be refused", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: error should mention %q: %v", c.name, c.says, err)
		}
	}

	// The other family is unaffected, which is the point of per-family modes: a
	// static IPv4 address alongside SLAAC IPv6 has to validate.
	if err := (HostIface{
		Iface: "eth1", Mode4: hostnet.ModeStatic, Mode6: hostnet.ModeSLAAC,
		Addrs: []string{"10.1.1.5/24"}, GW4: "10.1.1.1",
	}).Validate(); err != nil {
		t.Errorf("static IPv4 with SLAAC IPv6 should validate: %v", err)
	}
}

// An update that does not mention a mode has not asked to change it. Read from
// storage, an absent mode means static; arriving in an update it means "not
// mentioned" — and collapsing the two would turn every MTU edit into a switch
// back to static, taking the interface off DHCP because someone changed 1500 to
// 1400.
func TestPartialUpdateKeepsTheStoredMode(t *testing.T) {
	c := &Config{UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.SetHostIface(HostIface{
		Iface: "eth1", Mode4: hostnet.ModeDHCP, Mode6: hostnet.ModeSLAAC,
	}); err != nil {
		t.Fatal(err)
	}
	// An MTU-only edit, which is what the page sends for that cell.
	if err := c.SetHostIface(HostIface{Iface: "eth1", MTU: 1400}); err != nil {
		t.Fatal(err)
	}
	h := c.HostIfaceFor("eth1")
	if h.Mode4 != hostnet.ModeDHCP || h.Mode6 != hostnet.ModeSLAAC {
		t.Errorf("an MTU edit changed the modes: %+v", h)
	}
	if h.MTU != 1400 {
		t.Errorf("MTU = %d, want 1400", h.MTU)
	}

	// An update that does name a mode still changes it.
	if err := c.SetHostIface(HostIface{Iface: "eth1", Mode4: hostnet.ModeStatic, Mode6: hostnet.ModeSLAAC}); err != nil {
		t.Fatal(err)
	}
	if got := c.HostIfaceFor("eth1").Mode4; got != hostnet.ModeStatic {
		t.Errorf("Mode4 = %q, want static", string(got))
	}
}
