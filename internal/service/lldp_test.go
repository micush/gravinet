package service

import (
	"sort"
	"strings"
	"testing"

	"gravinet/internal/config"
)

func TestLLDPValidIface(t *testing.T) {
	valid := []string{"eth0", "enp0s3", "bond0.100", "br-lan", "eth0_1", "eth0@if2"}
	for _, s := range valid {
		if !ValidLLDPIface(s) {
			t.Errorf("ValidLLDPIface(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "eth0; rm -rf /", "eth 0", strings.Repeat("a", 16), "eth0\n"}
	for _, s := range invalid {
		if ValidLLDPIface(s) {
			t.Errorf("ValidLLDPIface(%q) = true, want false", s)
		}
	}
}

func TestLLDPConfigIsRunnableAndAnyCDP(t *testing.T) {
	var d config.DiscoveryConfig
	if d.IsRunnable() {
		t.Error("empty config should not be runnable")
	}
	d.Interfaces = []config.DiscoveryIface{{Name: "lo", LLDP: true, CDP: true}}
	if d.IsRunnable() {
		t.Error("loopback-only, even with both protocols on, should not be runnable")
	}
	if d.AnyCDP() {
		t.Error("loopback-only CDP should not count")
	}
	d.Interfaces = append(d.Interfaces, config.DiscoveryIface{Name: "eth0", LLDP: false, CDP: false})
	if d.IsRunnable() {
		t.Error("an interface with both protocols off should not make the config runnable")
	}
	d.Interfaces[1].LLDP = true
	if !d.IsRunnable() {
		t.Error("eth0 with LLDP on should make the config runnable")
	}
	if d.AnyCDP() {
		t.Error("no interface has CDP on yet")
	}
	d.Interfaces[1].CDP = true
	if !d.AnyCDP() {
		t.Error("eth0 now has CDP on")
	}
}

func TestLLDPArgsBuildsExpectedArgv(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.DiscoveryConfig
		want []string
	}{
		{"lldp only, one iface", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0", LLDP: true},
		}}, []string{"-d", "-I", "eth0"}},
		{"lldp+cdp, one iface", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0", LLDP: true, CDP: true},
		}}, []string{"-d", "-c", "-I", "eth0"}},
		{"cdp only counts as active too", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth1", CDP: true},
		}}, []string{"-d", "-c", "-I", "eth1"}},
		{"loopback excluded even if flagged", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "lo", LLDP: true, CDP: true},
			{Name: "eth0", LLDP: true},
		}}, []string{"-d", "-I", "eth0"}}, // no -c: lo's CDP must not count
		{"invalid iface name dropped, not injected", config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
			{Name: "eth0; rm -rf /", LLDP: true},
			{Name: "eth1", LLDP: true},
		}}, []string{"-d", "-I", "eth1"}},
		{"nothing active", config.DiscoveryConfig{}, []string{"-d"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lldpArgs(c.cfg)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("lldpArgs() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLLDPArgsMultipleIfacesJoinedWithCommaNotSpace(t *testing.T) {
	cfg := config.DiscoveryConfig{Interfaces: []config.DiscoveryIface{
		{Name: "eth0", LLDP: true},
		{Name: "eth1", LLDP: true},
	}}
	args := lldpArgs(cfg)
	// -I's value is a single argv token, comma-joined — never two separate
	// tokens, which would silently change lldpd's -I parsing.
	idx := -1
	for i, a := range args {
		if a == "-I" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("no -I flag found in %v", args)
	}
	val := args[idx+1]
	parts := strings.Split(val, ",")
	sort.Strings(parts)
	if strings.Join(parts, ",") != "eth0,eth1" {
		t.Errorf("-I value = %q, want a comma-joined \"eth0,eth1\" (in either order)", val)
	}
}

// TestParseLLDPNeighborsObjectShape covers lldpd's "interface as object"
// JSON shape, ported from a representative real lldpcli -f json output.
func TestParseLLDPNeighborsObjectShape(t *testing.T) {
	data := []byte(`{
		"lldp": {
			"interface": {
				"eth0": {
					"chassis": {
						"switch1.example": {
							"id": {"type": "mac", "value": "aa:bb:cc:dd:ee:ff"},
							"mgmt-ip": "10.0.0.1"
						}
					},
					"port": {
						"descr": "GigabitEthernet0/1"
					}
				}
			}
		}
	}`)
	rows, err := parseLLDPNeighborsJSON(data)
	if err != nil {
		t.Fatalf("parseLLDPNeighborsJSON: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.LocalIface != "eth0" {
		t.Errorf("LocalIface = %q, want eth0", r.LocalIface)
	}
	if r.SystemName != "switch1.example" {
		t.Errorf("SystemName = %q, want switch1.example", r.SystemName)
	}
	if r.Port != "GigabitEthernet0/1" {
		t.Errorf("Port = %q, want GigabitEthernet0/1", r.Port)
	}
	if r.MgmtIP != "10.0.0.1" {
		t.Errorf("MgmtIP = %q, want 10.0.0.1", r.MgmtIP)
	}
}

// TestParseLLDPNeighborsArrayShape covers the alternate "interface as
// array of single-key objects" shape parapet's own comment says some lldpd
// versions use instead.
func TestParseLLDPNeighborsArrayShape(t *testing.T) {
	data := []byte(`{
		"lldp": {
			"interface": [
				{"eth0": {
					"chassis": {"core-sw": {"mgmt-ip": "192.168.1.1"}},
					"port": {"id": {"type": "ifname", "value": "eth3"}}
				}},
				{"eth1": {
					"chassis": {"edge-sw": {}},
					"port": {}
				}}
			]
		}
	}`)
	rows, err := parseLLDPNeighborsJSON(data)
	if err != nil {
		t.Fatalf("parseLLDPNeighborsJSON: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byIface := map[string]LLDPNeighbor{}
	for _, r := range rows {
		byIface[r.LocalIface] = r
	}
	eth0, ok := byIface["eth0"]
	if !ok {
		t.Fatal("missing eth0 row")
	}
	if eth0.SystemName != "core-sw" || eth0.Port != "eth3" || eth0.MgmtIP != "192.168.1.1" {
		t.Errorf("eth0 row = %+v, want system=core-sw port=eth3 mgmt=192.168.1.1", eth0)
	}
	eth1, ok := byIface["eth1"]
	if !ok {
		t.Fatal("missing eth1 row")
	}
	// port has neither descr nor id.value, and chassis's inner object has no
	// recognized name field either — falls back to the sole map key, "edge-sw".
	if eth1.SystemName != "edge-sw" {
		t.Errorf("eth1 SystemName = %q, want edge-sw (fallback to sole chassis key)", eth1.SystemName)
	}
}

func TestParseLLDPNeighborsEmptyAndMalformed(t *testing.T) {
	rows, err := parseLLDPNeighborsJSON([]byte(`{"lldp": {}}`))
	if err != nil {
		t.Fatalf("unexpected error on empty lldp object: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %+v", rows)
	}
	if _, err := parseLLDPNeighborsJSON([]byte(`not json`)); err == nil {
		t.Error("expected an error parsing non-JSON input")
	}
}
