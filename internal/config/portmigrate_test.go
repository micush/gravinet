package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Every deployed node has a config in the pre-v789 shape, so the fold from
// primary/fallback to flat lists is the part of this change with the widest
// blast radius. A node that comes back up advertising a different port than it
// went down with is a partition, not a migration.

func loadJSON(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// The ordinary upgrade: primary + extras become one list, in order, so the
// port this node advertised before is still the one it advertises after.
func TestMigrateLegacyPortsPreservesOrder(t *testing.T) {
	c := loadJSON(t, `{
		"enable_ipv4": true,
		"primary_port": 51820,
		"extra_listen_ports": [443, 80],
		"tcp_fallback_port": 65432,
		"extra_tcp_listen_ports": [21, 23]
	}`)
	if got, want := c.UDPPortList(), []int{51820, 443, 80}; !reflect.DeepEqual(got, want) {
		t.Errorf("UDPPortList = %v, want %v", got, want)
	}
	if got, want := c.TCPPortList(), []int{65432, 21, 23}; !reflect.DeepEqual(got, want) {
		t.Errorf("TCPPortList = %v, want %v", got, want)
	}
	if got := c.AdvertisedUDPPort(); got != 51820 {
		t.Errorf("AdvertisedUDPPort = %d, want the old primary 51820 — an upgraded node must advertise what it advertised before", got)
	}
}

// primary_port 0 meant "UDP off" and must stay off. Defaulting it here would
// silently open a listener the operator had deliberately closed.
func TestMigrateLegacyUDPOffStaysOff(t *testing.T) {
	c := loadJSON(t, `{"enable_ipv4": true, "primary_port": 0, "tcp_fallback_port": 65432}`)
	if c.UDPEnabled() {
		t.Errorf("udp ports = %v, want empty — primary_port 0 meant off", c.UDPPortList())
	}
	if !c.TCPEnabled() {
		t.Error("tcp should still be on")
	}
}

// disable_tcp_fallback was the TCP off switch, and is a different field from
// the port — so an off config usually still carries a port value. Reading the
// port and ignoring the bool would turn TCP back on.
func TestMigrateLegacyTCPDisabledStaysOff(t *testing.T) {
	c := loadJSON(t, `{
		"enable_ipv4": true,
		"primary_port": 51820,
		"tcp_fallback_port": 443,
		"disable_tcp_fallback": true
	}`)
	if c.TCPEnabled() {
		t.Errorf("tcp ports = %v, want empty — disable_tcp_fallback was set", c.TCPPortList())
	}
	if got := c.AdvertisedUDPPort(); got != 51820 {
		t.Errorf("AdvertisedUDPPort = %d, want 51820", got)
	}
}

// tcp_fallback_port 0 meant "use the default", not "off" — the two were
// distinguished by the separate bool. Treating 0 as off would take TCP away
// from every node that never set the port explicitly, which is most of them.
func TestMigrateLegacyTCPZeroMeansDefaultNotOff(t *testing.T) {
	c := loadJSON(t, `{"enable_ipv4": true, "primary_port": 51820}`)
	if got, want := c.TCPPortList(), []int{DefaultTCPFallbackPort}; !reflect.DeepEqual(got, want) {
		t.Errorf("TCPPortList = %v, want %v — an absent tcp_fallback_port meant the default", got, want)
	}
}

// A config already in the flat shape wins outright; legacy keys alongside it
// are ignored rather than merged, so a hand-edited file doesn't end up with a
// union nobody asked for.
func TestMigrateFlatConfigWinsOverLegacyKeys(t *testing.T) {
	c := loadJSON(t, `{
		"enable_ipv4": true,
		"udp_ports": [1234],
		"tcp_ports": [5678],
		"primary_port": 51820,
		"extra_listen_ports": [443],
		"tcp_fallback_port": 65432
	}`)
	if got, want := c.UDPPortList(), []int{1234}; !reflect.DeepEqual(got, want) {
		t.Errorf("UDPPortList = %v, want %v — the flat list must win, not merge", got, want)
	}
	if got, want := c.TCPPortList(), []int{5678}; !reflect.DeepEqual(got, want) {
		t.Errorf("TCPPortList = %v, want %v", got, want)
	}
}

// Round-trip: a migrated config saved by this build carries only the flat
// form, and reloading it is a no-op. If the legacy keys survived a save they
// would go on shadowing the flat ones on every subsequent load.
func TestMigratedConfigRoundTripsFlat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{
		"enable_ipv4": true,
		"primary_port": 51820,
		"extra_listen_ports": [443],
		"tcp_fallback_port": 65432
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SaveTo(p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"primary_port", "extra_listen_ports", "tcp_fallback_port", "disable_tcp_fallback", "extra_tcp_listen_ports"} {
		if containsKey(string(raw), k) {
			t.Errorf("saved config still carries the legacy key %q", k)
		}
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.UDPPortList(), c2.UDPPortList()) || !reflect.DeepEqual(c.TCPPortList(), c2.TCPPortList()) {
		t.Fatalf("round trip changed the ports: %v/%v then %v/%v",
			c.UDPPortList(), c.TCPPortList(), c2.UDPPortList(), c2.TCPPortList())
	}
}

func containsKey(body, key string) bool {
	q := `"` + key + `"`
	for i := 0; i+len(q) <= len(body); i++ {
		if body[i:i+len(q)] == q {
			return true
		}
	}
	return false
}
