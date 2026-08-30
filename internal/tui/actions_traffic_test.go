package tui

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// trafficTestSnapshot extends testSnapshot with the Traffic-group fixtures
// each test below needs: one NAT rule, one QoS rule, one shaped interface,
// one advertised route, one BGP neighbor, one live firewall rule, one RA
// interface.
func trafficTestSnapshot() *snapshot {
	s := testSnapshot()
	s.cfg.NAT.Enabled = true
	s.cfg.NAT.Rules = []config.NATRule{
		{Source: "10.42.0.0/16", Translate: "masquerade", Enabled: true},
	}
	s.cfg.QoS.Enabled = true
	s.cfg.QoS.Classes = 3
	s.cfg.QoS.Rules = []config.QoSRule{
		{Protocol: "tcp", PortMin: 22, PortMax: 22, Class: 0},
	}
	s.cfg.Shaping = []config.IfaceShaping{{Iface: "eth0"}}
	s.cfg.Networks[0].Routes = []config.Route{{CIDR: "192.168.5.0/24", Metric: 10, Enabled: true}}
	s.cfg.BGP.Neighbors = []config.BGPNeighbor{{Peer: "10.0.0.1", RemoteAS: 65001}}
	s.cfg.RouterAdvert.Interfaces = []config.RAInterface{{Iface: "eth1"}}
	s.firewall = []mesh.FirewallRule{{ID: 7, Action: "allow", Scope: "corp"}}
	return s
}

func trafficTestModel(t *testing.T, section string) (*model, *fakeGravinet) {
	t.Helper()
	f := installFakeGravinet(t)
	m := newModel(trafficTestSnapshot(), "dark", colorMono)
	m.w, m.h = 120, 40
	m.setSection(section)
	return m, f
}

// ---- nat ------------------------------------------------------------------

func TestNATAddBareIfaceArgv(t *testing.T) {
	m, f := trafficTestModel(t, "nat")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"iface": "eth0"})
	hasArgs(t, lastCall(f), "nat", "add", "eth0")
}

func TestNATAddKeywordFormArgv(t *testing.T) {
	m, f := trafficTestModel(t, "nat")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"source": "10.0.0.0/8", "translate": "masquerade"})
	got := lastCall(f)
	hasArgs(t, got, "nat", "add", "source", "10.0.0.0/8", "translate", "masquerade")
	// kw() reads bare tokens, not flags — confirm no stray "-source" leaked in.
	for _, a := range got {
		if a == "-source" || a == "-translate" {
			t.Errorf("nat add must use bare keyword tokens, not flags: %v", got)
		}
	}
}

func TestNATDeleteArgv(t *testing.T) {
	m, f := trafficTestModel(t, "nat")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "nat", "delete", "0")
}

func TestNATToggleArgv(t *testing.T) {
	m, f := trafficTestModel(t, "nat")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "nat", "disable-rule", "0")
}

func TestNATStateFormArgv(t *testing.T) {
	m, f := trafficTestModel(t, "nat")
	openMnemonicForm(t, m, "state")
	submitCurrentForm(t, m, map[string]string{"on": "false"})
	hasArgs(t, lastCall(f), "nat", "disable")
}

// ---- qos --------------------------------------------------------------

func TestQoSAddProtoPortArgv(t *testing.T) {
	m, f := trafficTestModel(t, "qos")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"proto": "tcp", "port": "3389", "priority": "highest"})
	got := lastCall(f)
	hasArgs(t, got, "qos", "add", "tcp", "3389", "priority", "highest")
}

func TestQoSAddServicesArgv(t *testing.T) {
	m, f := trafficTestModel(t, "qos")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"services": "ssh,rdp", "priority": "high"})
	hasArgs(t, lastCall(f), "qos", "add", "service", "ssh,rdp", "priority", "high")
}

func TestQoSAddWithScopeUsesBareKeywordNotFlag(t *testing.T) {
	m, f := trafficTestModel(t, "qos")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"proto": "udp", "port": "500", "priority": "low", "scope": "corp"})
	got := lastCall(f)
	hasArgs(t, got, "qos", "add", "udp", "500", "priority", "low", "scope", "corp")
	for _, a := range got {
		if a == "-scope" {
			t.Errorf("qos scope must be a bare keyword (kw()), not a flag: %v", got)
		}
	}
}

func TestQoSDeleteArgvRebuildsMatchFromRuleData(t *testing.T) {
	m, f := trafficTestModel(t, "qos")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "qos", "delete", "tcp", "22")
}

func TestQoSToggleArgv(t *testing.T) {
	m, f := trafficTestModel(t, "qos")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "qos", "disable-rule", "tcp", "22")
}

// ---- bandwidth --------------------------------------------------------

func TestBandwidthAddArgv(t *testing.T) {
	m, f := trafficTestModel(t, "bandwidth")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"iface": "eth2"})
	hasArgs(t, lastCall(f), "bandwidth", "add", "-iface", "eth2")
}

func TestBandwidthDeleteArgv(t *testing.T) {
	m, f := trafficTestModel(t, "bandwidth")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "bandwidth", "del", "-iface", "eth0")
}

func TestBandwidthToggleArgv(t *testing.T) {
	m, f := trafficTestModel(t, "bandwidth")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "bandwidth", "enable", "-iface", "eth0")
}

func TestBandwidthEditRateArgvUsesMbpsNotRawBytes(t *testing.T) {
	// Regression test: the stored value is bytes/sec but config.ParseRate
	// (what the CLI actually parses) expects bits/sec with a unit suffix.
	// Submitting the pre-filled default must never send a bare byte count.
	m, f := trafficTestModel(t, "bandwidth")
	m.dispatchRowAction('e')
	if m.form == nil {
		t.Fatal("'e' should open the rate-edit form")
	}
	submitCurrentForm(t, m, map[string]string{"up": "10Mbps", "down": "5Mbps"})
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(f.calls), f.calls)
	}
	hasArgs(t, f.calls[0], "bandwidth", "up", "10Mbps", "-iface", "eth0")
	hasArgs(t, f.calls[1], "bandwidth", "down", "5Mbps", "-iface", "eth0")
}

func TestBandwidthEditUnlimitedSendsZero(t *testing.T) {
	m, f := trafficTestModel(t, "bandwidth")
	m.snap.cfg.Shaping[0].UpBytesPerSec = 1250000 // 10Mbps, so "up" differs from the unlimited default
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"up": "unlimited"})
	hasArgs(t, lastCall(f), "bandwidth", "up", "0", "-iface", "eth0")
}

// ---- routes -------------------------------------------------------------

func TestRoutesAddArgv(t *testing.T) {
	m, f := trafficTestModel(t, "routes")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"net": "corp", "cidr": "172.16.0.0/12", "metric": "5"})
	hasArgs(t, lastCall(f), "route", "add", "172.16.0.0/12", "-net", "corp", "-metric", "5")
}

func TestRoutesDeleteArgv(t *testing.T) {
	m, f := trafficTestModel(t, "routes")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "route", "delete", "192.168.5.0/24", "-net", "corp")
}

func TestRoutesToggleArgv(t *testing.T) {
	m, f := trafficTestModel(t, "routes")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "route", "disable", "192.168.5.0/24", "-net", "corp")
}

// ---- bgp ----------------------------------------------------------------

func TestBGPNeighborDeleteArgv(t *testing.T) {
	m, f := trafficTestModel(t, "bgp")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "traffic", "bgp", "neighbor", "del", "10.0.0.1")
}

func TestBGPNeighborEditArgvIsAnUpsertCarryingRemoteAS(t *testing.T) {
	m, f := trafficTestModel(t, "bgp")
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"description": "core router", "bfd": "true"})
	got := lastCall(f)
	hasArgs(t, got, "traffic", "bgp", "neighbor", "add", "10.0.0.1", "65001", "-description", "core router", "-bfd")
}

func TestBGPToggleShutdownIsAnUpsert(t *testing.T) {
	m, f := trafficTestModel(t, "bgp")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "traffic", "bgp", "neighbor", "add", "10.0.0.1", "65001", "-shutdown")
}

func TestBGPSetOnlySendsChangedFlags(t *testing.T) {
	m, f := trafficTestModel(t, "bgp")
	openMnemonicForm(t, m, "router id")
	submitCurrentForm(t, m, map[string]string{"router_id": "10.9.9.9"})
	got := lastCall(f)
	hasArgs(t, got, "traffic", "bgp", "set", "-router-id", "10.9.9.9")
	for _, a := range got {
		if a == "-asn" || a == "-keepalive" || a == "-hold" || a == "-auto-bgp" {
			t.Errorf("unchanged bgp set fields should not appear: %v", got)
		}
	}
}

func TestBGPEnableStateArgv(t *testing.T) {
	m, f := trafficTestModel(t, "bgp")
	openMnemonicForm(t, m, "state")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "traffic", "bgp", "enable")
}

// ---- firewall -----------------------------------------------------------

func TestFirewallAddArgvUsesFlagsNotPositionalAction(t *testing.T) {
	// Regression test: cmdFW's "add" verb takes every field as a flag,
	// including -action — there is no positional form at all.
	m, f := trafficTestModel(t, "firewall")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"action": "deny", "direction": "in", "proto": "tcp", "source": "10.0.0.0/8"})
	got := lastCall(f)
	hasArgs(t, got, "fw", "add", "-action", "deny", "-dir", "in", "-proto", "tcp", "-src", "10.0.0.0/8", "-sock")
	if got[2] != "-action" {
		t.Errorf("action must be passed as -action, not positionally; got %v", got)
	}
	// Regression: fw is control-socket-only; its own flag.FlagSet never
	// registers -config, so appending one is an immediate parse error.
	hasNotArgs(t, got, "-config")
}

func TestFirewallDeleteArgvCarriesScope(t *testing.T) {
	m, f := trafficTestModel(t, "firewall")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	got := lastCall(f)
	hasArgs(t, got, "fw", "del", "7", "-scope", "corp", "-sock")
	hasNotArgs(t, got, "-config")
}

func TestFirewallDeleteIDIsPositional(t *testing.T) {
	m, f := trafficTestModel(t, "firewall")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	got := lastCall(f)
	if len(got) < 3 || got[2] != "7" {
		t.Errorf("the rule id must be positional (splitIDs reads it off rest directly), got %v", got)
	}
}

// ---- ipv6ra -------------------------------------------------------------

func TestIPv6RAToggleArgv(t *testing.T) {
	m, f := trafficTestModel(t, "ipv6ra")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "traffic", "ipv6ra", "disable", "eth1")
}

func TestIPv6RAHasNoAddAction(t *testing.T) {
	// Deliberate boundary: adding a full RA interface entry needs
	// validation this package doesn't reproduce (see cmdIPv6RA's own
	// comment). Confirm 'a' does nothing rather than silently succeeding
	// with an incomplete entry.
	m, _ := trafficTestModel(t, "ipv6ra")
	m.dispatchAdd()
	if m.form != nil {
		t.Error("ipv6ra should have no add form")
	}
	if m.flash == "" {
		t.Error("dispatchAdd should explain why nothing happened")
	}
}
