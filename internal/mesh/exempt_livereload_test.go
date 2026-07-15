package mesh

import (
	"net/netip"
	"testing"

	"gravinet/internal/crypto"
)

// TestExemptLiveReload proves the firewall exemption allowlist is configurable
// and applies LIVE through ReloadRuntime (the same path the web API/CLI use):
// swapping the list immediately changes which protocols punch through a
// deny-all, and FirewallExemptsFor reports the active set.
func TestExemptLiveReload(t *testing.T) {
	const netID = uint64(0xEEE1)
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev("A")
	eng := NewEngine(Options{
		NodeID: "A", Hostname: "A", WebPort: 8443,
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: dev,
			Self4: netip.MustParseAddr("10.7.0.1"), Subnet4: netip.MustParsePrefix("10.7.0.0/16"),
			FirewallEnabled: true,
			FirewallRules:   []FirewallRule{{Action: "deny"}}, // deny-all
			FirewallExempts: []FirewallExempt{{Name: "BGP", Proto: 6, Port: 179}},
		}},
	})

	ns := eng.network(netID)
	if ns == nil {
		t.Fatal("network missing")
	}

	bgp := mkL4Ports(6, 5000, 179)
	rip := mkL4Ports(17, 520, 520)
	ordinary := mkL4Ports(6, 5000, 8080)

	// Initial: only BGP is exempt.
	if !ns.fw.allow(fwIn, bgp) {
		t.Error("BGP should be exempt initially")
	}
	if ns.fw.allow(fwIn, rip) {
		t.Error("RIP should NOT be exempt initially")
	}
	if ns.fw.allow(fwIn, ordinary) {
		t.Error("ordinary traffic must be blocked by deny-all")
	}
	if got := eng.FirewallExemptsFor(netID); len(got) != 1 || got[0].Name != "BGP" {
		t.Errorf("FirewallExemptsFor = %+v, want one BGP entry", got)
	}

	// Live reload to a different allowlist: RIP instead of BGP.
	if err := eng.ReloadRuntime(netID, NetSpec{
		ID: netID, FirewallEnabled: true,
		FirewallRules:   []FirewallRule{{Action: "deny"}},
		FirewallExempts: []FirewallExempt{{Name: "RIP", Proto: 17, Port: 520}},
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if ns.fw.allow(fwIn, bgp) {
		t.Error("after reload, BGP should no longer be exempt")
	}
	if !ns.fw.allow(fwIn, rip) {
		t.Error("after reload, RIP should be exempt")
	}
	if got := eng.FirewallExemptsFor(netID); len(got) != 1 || got[0].Name != "RIP" {
		t.Errorf("after reload FirewallExemptsFor = %+v, want one RIP entry", got)
	}

	// Management exemption follows the node's web-admin port.
	if err := eng.ReloadRuntime(netID, NetSpec{
		ID: netID, FirewallEnabled: true,
		FirewallRules:   []FirewallRule{{Action: "deny"}},
		FirewallExempts: []FirewallExempt{{Name: "management", Proto: 6, Mgmt: true}},
	}); err != nil {
		t.Fatalf("reload2: %v", err)
	}
	if !ns.fw.allow(fwIn, mkL4Ports(6, 5000, 8443)) {
		t.Error("management exemption should follow webPort 8443")
	}
	if ns.fw.allow(fwIn, mkL4Ports(6, 5000, 179)) {
		t.Error("BGP must not be exempt once the list is only management")
	}
}
