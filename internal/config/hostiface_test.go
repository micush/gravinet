package config

import (
	"encoding/json"
	"strings"
	"testing"
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
