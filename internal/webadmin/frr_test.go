package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
)

func frrHas(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("expected to contain %q\n--- got ---\n%s", needle, hay)
	}
}

func frrLacks(t *testing.T, hay, needle string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("expected NOT to contain %q\n--- got ---\n%s", needle, hay)
	}
}

func daemonSet(b config.BGPConfig) map[string]bool {
	m := map[string]bool{}
	for _, d := range neededDaemons(b) {
		m[d] = true
	}
	return m
}

// A minimal enabled BGP config must still emit a runnable `router bgp <asn>`
// block and request bgpd, even with nothing else filled in. Ported from
// parapet's bgp_minimal_block test.
func TestFRRBGPMinimalBlock(t *testing.T) {
	b := config.BGPConfig{Enabled: true, ASN: 65001}
	c := renderFRR(b)
	frrHas(t, c, "router bgp 65001\n")
	frrLacks(t, c, "bgp router-id") // no router-id line since field is empty
	if !daemonSet(b)["bgpd"] {
		t.Error("bgpd must be requested purely from enabled+asn")
	}
	// Disabled → no BGP block, bgpd not requested.
	d := renderFRR(config.BGPConfig{Enabled: false, ASN: 65001})
	frrLacks(t, d, "router bgp")
	if daemonSet(config.BGPConfig{Enabled: false, ASN: 65001})["bgpd"] {
		t.Error("bgpd must not be requested when disabled")
	}
}

// Password + BFD: an MD5 password is emitted, the global BFD toggle implies bfd
// on every neighbor, and bfdd is requested. Ported from parapet's
// bgp_password_and_bfd test.
func TestFRRBGPPasswordAndBFD(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001, RouterID: "1.2.3.4", BFD: true,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002, Password: "s3cr3t", BFD: false},
			{Peer: "10.0.0.3", RemoteAS: 65003, BFD: true},
		},
	}
	c := renderFRR(b)
	frrHas(t, c, " bgp router-id 1.2.3.4\n")
	frrHas(t, c, " neighbor 10.0.0.2 remote-as 65002\n")
	frrHas(t, c, " neighbor 10.0.0.2 password s3cr3t\n")
	// Global BFD implies bfd on all neighbors.
	frrHas(t, c, " neighbor 10.0.0.2 bfd\n")
	frrHas(t, c, " neighbor 10.0.0.3 bfd\n")
	// address-family activation for each neighbor.
	frrHas(t, c, "  neighbor 10.0.0.2 activate\n")
	ds := daemonSet(b)
	if !ds["bgpd"] || !ds["bfdd"] {
		t.Errorf("expected bgpd+bfdd, got %v", ds)
	}
}

// A single neighbor with BFD (no global toggle) still requests bfdd. Guards the
// per-neighbor branch of neededDaemons.
func TestFRRPerNeighborBFDRequestsBfdd(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, BFD: true}},
	}
	if !daemonSet(b)["bfdd"] {
		t.Error("a single BFD neighbor must request bfdd")
	}
	// No BFD anywhere → no bfdd.
	nb := config.BGPConfig{Enabled: true, ASN: 65001, Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}}}
	if daemonSet(nb)["bfdd"] {
		t.Error("bfdd must not be requested with no BFD configured")
	}
}

// Whitespace in a password is stripped before it's emitted (it would otherwise
// break the conf line). Ported from parapet's bgp_password_whitespace_stripped.
func TestFRRBGPPasswordWhitespaceStripped(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, Password: "bad pw"}},
	}
	frrHas(t, renderFRR(b), " neighbor 10.0.0.2 password badpw\n")
}

// Injection attempts in user-supplied tokens are filtered, never emitted.
// Ported from parapet's injection_is_filtered.
func TestFRRInjectionFiltered(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		// A network prefix carrying an injected directive must be rejected by
		// safeToken (it contains spaces and a ';'), so nothing leaks.
		Networks:  []string{"10.0.0.0/24 ; shutdown"},
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2 ; clear ip bgp", RemoteAS: 65002}},
	}
	c := renderFRR(b)
	frrLacks(t, c, "shutdown")
	frrLacks(t, c, "clear ip bgp")
	// A clean network on the same config still renders.
	b.Networks = append(b.Networks, "10.1.0.0/16")
	frrHas(t, renderFRR(b), "  network 10.1.0.0/16\n")
}

// A neighbor with a zero remote-as or empty peer is skipped entirely (matches
// parapet's render guards) rather than emitting a broken line.
func TestFRRSkipsInvalidNeighbors(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 0},     // zero AS — skip
			{Peer: "", RemoteAS: 65003},         // empty peer — skip
			{Peer: "10.0.0.4", RemoteAS: 65004}, // valid
		},
	}
	c := renderFRR(b)
	frrLacks(t, c, "neighbor 10.0.0.2")
	frrHas(t, c, " neighbor 10.0.0.4 remote-as 65004\n")
}

func TestFRRNetworksAndRedistribute(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Networks:              []string{"10.0.0.0/24", "192.168.1.0/24"},
		RedistributeConnected: true, RedistributeStatic: true,
	}
	c := renderFRR(b)
	frrHas(t, c, "  network 10.0.0.0/24\n")
	frrHas(t, c, "  network 192.168.1.0/24\n")
	frrHas(t, c, "  redistribute connected\n")
	frrHas(t, c, "  redistribute static\n")
	frrHas(t, c, " address-family ipv4 unicast\n")
	frrHas(t, c, " exit-address-family\n")
}

// syncDaemonsContent flips the managed daemons to match the wanted set and
// leaves unmanaged lines untouched. Ported from parapet's sync_daemons logic.
func TestSyncDaemonsContent(t *testing.T) {
	// A representative /etc/frr/daemons: bgpd/bfdd off, staticd on, plus a
	// comment and an unmanaged setting line that must be preserved verbatim.
	existing := "# frr daemons\n" +
		"bgpd=no\n" +
		"ospfd=no\n" +
		"bfdd=no\n" +
		"staticd=yes\n" +
		"vtysh_enable=yes\n"
	want := neededDaemons(config.BGPConfig{Enabled: true, ASN: 65001, BFD: true}) // staticd, bgpd, bfdd

	body, changed := syncDaemonsContent(existing, want)
	if !changed {
		t.Fatal("expected a change (bgpd/bfdd flipped on)")
	}
	frrHas(t, body, "bgpd=yes\n")
	frrHas(t, body, "bfdd=yes\n")
	frrHas(t, body, "staticd=yes\n")
	frrHas(t, body, "ospfd=no\n") // wanted-out managed daemon stays off
	// Non-managed lines are preserved exactly.
	frrHas(t, body, "# frr daemons\n")
	frrHas(t, body, "vtysh_enable=yes\n")

	// Idempotent: re-running with the same want yields no change.
	body2, changed2 := syncDaemonsContent(body, want)
	if changed2 {
		t.Error("second sync with same want must be a no-op")
	}
	if body2 != body {
		t.Error("idempotent sync changed the body")
	}

	// Turning BGP off flips bgpd/bfdd back to =no.
	off := neededDaemons(config.BGPConfig{Enabled: false})
	body3, changed3 := syncDaemonsContent(body, off)
	if !changed3 {
		t.Error("disabling BGP should flip bgpd/bfdd off")
	}
	frrHas(t, body3, "bgpd=no\n")
	frrHas(t, body3, "bfdd=no\n")
	frrHas(t, body3, "staticd=yes\n") // staticd always wanted
}

func TestSafeToken(t *testing.T) {
	good := []string{"10.0.0.1", "fd00::2", "10.0.0.0/24", "eth0", "peer-1_a"}
	bad := []string{"", "has space", "semi;colon", "back`tick", "pipe|x", strings.Repeat("x", 65)}
	for _, g := range good {
		if !safeToken(g) {
			t.Errorf("safeToken(%q) = false, want true", g)
		}
	}
	for _, b := range bad {
		if safeToken(b) {
			t.Errorf("safeToken(%q) = true, want false", b)
		}
	}
}
