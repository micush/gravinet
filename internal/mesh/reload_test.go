package mesh

import (
	"net"
	"net/netip"
	"testing"

	"gravinet/internal/crypto"
)

func newReloadTestEngine(t *testing.T, netID uint64) (*Engine, *netState) {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{mustKey()})
	dev := newFakeDev("rl")
	eng := NewEngine(Options{
		NodeID: "A", Hostname: "A",
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.9.0.1")}},
	})
	eng.Attach(nopSender{})
	return eng, eng.network(netID)
}

func TestReloadRuntimeSwapsThrottleAndNAT(t *testing.T) {
	const netID = uint64(0xABCD)
	eng, ns := newReloadTestEngine(t, netID)

	if ns.egress.Load() != nil || ns.ingress.Load() != nil || ns.nat.Load() != nil {
		t.Fatal("expected no throttle/nat on a fresh network")
	}

	// Turn on up/down throttle + NAT (explicit translate IP) live.
	spec := NetSpec{
		ID: netID, ThrottleUp: 1_000_000, ThrottleDown: 2_000_000,
		NATEnabled: true,
		NAT:        []NATRuleSpec{{Direction: "overlay2underlay", Translate: "192.0.2.1"}},
	}
	if err := eng.ReloadRuntime(netID, spec); err != nil {
		t.Fatalf("reload on: %v", err)
	}
	eg := ns.egress.Load()
	if eg == nil {
		t.Error("egress should be active after enabling up-throttle")
	}
	if ns.ingress.Load() == nil {
		t.Error("ingress should be active after enabling down-throttle")
	}
	if ns.nat.Load() == nil {
		t.Error("nat should be active after enabling NAT")
	}

	// Changing the rate must replace the shaper instance (and retire the old).
	spec.ThrottleUp = 5_000_000
	if err := eng.ReloadRuntime(netID, spec); err != nil {
		t.Fatalf("reload rate change: %v", err)
	}
	if ns.egress.Load() == eg {
		t.Error("egress shaper should be replaced when the rate changes")
	}

	// Disable everything live.
	if err := eng.ReloadRuntime(netID, NetSpec{ID: netID}); err != nil {
		t.Fatalf("reload off: %v", err)
	}
	if ns.egress.Load() != nil || ns.ingress.Load() != nil || ns.nat.Load() != nil {
		t.Error("throttle/nat should be cleared after a disabling reload")
	}

	if err := eng.ReloadRuntime(0xDEAD, NetSpec{ID: 0xDEAD}); err == nil {
		t.Error("expected an error reloading an unknown network")
	}
}

// TestNATMasqueradeInterface checks that a masquerade rule scoped to a real
// interface resolves that interface's IPv4 as the translate address.
func TestNATMasqueradeInterface(t *testing.T) {
	// Find a usable non-loopback IPv4 interface in this environment.
	ifaces, _ := net.Interfaces()
	var name string
	for _, ifc := range ifaces {
		if ip, ok := interfaceIPv4(ifc.Name); ok && ip.IsValid() {
			name = ifc.Name
			break
		}
	}
	if name == "" {
		t.Skip("no non-loopback IPv4 interface available")
	}
	const netID = uint64(0xBEEF)
	eng, ns := newReloadTestEngine(t, netID)
	spec := NetSpec{
		ID: netID, NATEnabled: true,
		NAT: []NATRuleSpec{{Direction: "overlay2underlay", Translate: "masquerade", Interface: name}},
	}
	if err := eng.ReloadRuntime(netID, spec); err != nil {
		t.Fatal(err)
	}
	if ns.nat.Load() == nil {
		t.Errorf("masquerade NAT on %s should produce an active table", name)
	}
}
