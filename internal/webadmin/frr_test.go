package webadmin

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

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

// enableDaemonsContent flips bgpd/bfdd from =no to =yes on FRR detection while
// leaving every other line — including other managed daemons and unrelated
// settings — exactly as found, and never disables anything.
func TestEnableDaemonsContent(t *testing.T) {
	// A stock /etc/frr/daemons: everything off, plus a comment, an already-on
	// unrelated daemon, and an unmanaged setting line that must survive verbatim.
	existing := "# frr daemons\n" +
		"bgpd=no\n" +
		"ospfd=no\n" +
		"bfdd=no\n" +
		"staticd=yes\n" +
		"vtysh_enable=yes\n"

	body, changed := enableDaemonsContent(existing, frrBGPBFDDaemons)
	if !changed {
		t.Fatal("expected a change (bgpd/bfdd flipped on)")
	}
	frrHas(t, body, "bgpd=yes\n")
	frrHas(t, body, "bfdd=yes\n")
	// Only bgpd/bfdd are touched: ospfd stays off, and nothing else moves.
	frrHas(t, body, "ospfd=no\n")
	frrHas(t, body, "staticd=yes\n")
	frrHas(t, body, "# frr daemons\n")
	frrHas(t, body, "vtysh_enable=yes\n")

	// Idempotent: a second pass with them already on reports no change.
	body2, changed2 := enableDaemonsContent(body, frrBGPBFDDaemons)
	if changed2 {
		t.Error("second pass with bgpd/bfdd already on must be a no-op")
	}
	if body2 != body {
		t.Error("idempotent enable changed the body")
	}

	// One already on, one off: still a change, and it only enables (never the
	// reverse).
	mixed := "bgpd=yes\nbfdd=no\n"
	out, changed3 := enableDaemonsContent(mixed, frrBGPBFDDaemons)
	if !changed3 {
		t.Error("bfdd=no should flip to yes")
	}
	frrHas(t, out, "bgpd=yes\n")
	frrHas(t, out, "bfdd=yes\n")
	frrLacks(t, out, "bgpd=no")
	frrLacks(t, out, "bfdd=no")
}

// The exact bug: FRR has an existing BGP config with established peers, gravinet
// has none. parseRunningConfigBGP must reconstruct that config from FRR's
// `show running-config` output so the page reflects reality instead of empty.
func TestParseRunningConfigBGP(t *testing.T) {
	// Representative FRR running-config: a router bgp stanza with router-id, two
	// neighbors (one with description + password + bfd), an address-family with
	// networks/redistribute/activate, wrapped in surrounding unrelated config.
	rc := "frr version 8.4\n" +
		"!\n" +
		"router bgp 65001\n" +
		" bgp router-id 10.0.0.1\n" +
		" neighbor 10.0.0.2 remote-as 65002\n" +
		" neighbor 10.0.0.2 description core uplink\n" +
		" neighbor 10.0.0.2 password s3cr3t\n" +
		" neighbor 10.0.0.2 bfd\n" +
		" neighbor 10.0.0.3 remote-as 65003\n" +
		" !\n" +
		" address-family ipv4 unicast\n" +
		"  network 10.0.0.0/24\n" +
		"  network 192.168.5.0/24\n" +
		"  redistribute connected\n" +
		"  neighbor 10.0.0.2 activate\n" +
		"  neighbor 10.0.0.3 activate\n" +
		" exit-address-family\n" +
		"exit\n" +
		"!\n" +
		"line vty\n" +
		"!\n"

	cfg, hasPw, ok := parseRunningConfigBGP(rc)
	if !ok {
		t.Fatal("expected to find a BGP stanza")
	}
	if !cfg.Enabled || cfg.ASN != 65001 {
		t.Errorf("asn/enabled wrong: %+v", cfg)
	}
	if cfg.RouterID != "10.0.0.1" {
		t.Errorf("router-id = %q, want 10.0.0.1", cfg.RouterID)
	}
	if !cfg.RedistributeConnected || cfg.RedistributeStatic {
		t.Errorf("redistribute flags wrong: conn=%v static=%v", cfg.RedistributeConnected, cfg.RedistributeStatic)
	}
	if len(cfg.Networks) != 2 || cfg.Networks[0] != "10.0.0.0/24" || cfg.Networks[1] != "192.168.5.0/24" {
		t.Errorf("networks wrong: %+v", cfg.Networks)
	}
	if len(cfg.Neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2 (activate lines must not create extras): %+v", len(cfg.Neighbors), cfg.Neighbors)
	}
	n0 := cfg.Neighbors[0]
	if n0.Peer != "10.0.0.2" || n0.RemoteAS != 65002 || n0.Description != "core uplink" || !n0.BFD {
		t.Errorf("neighbor[0] wrong: %+v", n0)
	}
	if n0.Password != "" {
		t.Errorf("password must not be imported, got %q", n0.Password)
	}
	if !hasPw {
		t.Error("hasPasswords should be true (one neighbor had a password line)")
	}
	if cfg.Neighbors[1].Peer != "10.0.0.3" || cfg.Neighbors[1].RemoteAS != 65003 {
		t.Errorf("neighbor[1] wrong: %+v", cfg.Neighbors[1])
	}
}

// No BGP stanza → ok=false, so the handler shows an empty editor rather than a
// bogus enabled config.
func TestParseRunningConfigBGPNone(t *testing.T) {
	for _, rc := range []string{"", "frr version 8.4\n!\nline vty\n!\n", "interface eth0\n ip address 10.0.0.1/24\n!\n"} {
		if _, _, ok := parseRunningConfigBGP(rc); ok {
			t.Errorf("expected no BGP stanza for %q", rc)
		}
	}
}

// The stanza must not bleed into following top-level config: a neighbor-like
// line after the stanza ends is ignored.
func TestParseRunningConfigBGPStanzaBoundary(t *testing.T) {
	rc := "router bgp 65001\n" +
		" neighbor 10.0.0.2 remote-as 65002\n" +
		"exit\n" +
		"router ospf\n" +
		" network 10.9.9.0/24 area 0\n" +
		"!\n"
	cfg, _, ok := parseRunningConfigBGP(rc)
	if !ok || cfg.ASN != 65001 {
		t.Fatalf("bgp parse failed: %+v", cfg)
	}
	if len(cfg.Neighbors) != 1 {
		t.Errorf("stanza boundary leaked: got %d neighbors, want 1", len(cfg.Neighbors))
	}
	for _, n := range cfg.Networks {
		if n == "10.9.9.0/24 area 0" || n == "10.9.9.0/24" {
			t.Error("OSPF network leaked into BGP import")
		}
	}
}

func TestRunVtyshAbsent(t *testing.T) {
	// With no vtysh present, runVtysh must return promptly with ok=false and
	// never spawn a process or block — this is what keeps the BGP endpoints
	// fast on hosts without FRR.
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })
	done := make(chan struct{})
	go func() {
		if out, ok := runVtysh("show running-config"); ok || out != nil {
			t.Errorf("runVtysh with no vtysh = (%v,%v), want (nil,false)", out, ok)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runVtysh did not return promptly when vtysh is absent")
	}
	// importBGPFromFRR builds on it and must also report nothing to import.
	if _, _, ok := importBGPFromFRR(); ok {
		t.Error("importBGPFromFRR should report ok=false when vtysh is absent")
	}
}

// The reported mismatch: FRR has a live BGP session (32-bit ASNs) but the
// editor showed empty. summaryToBGPConfig must rebuild the config from the same
// summary JSON the live-peers panel uses, so the editor matches. Values here
// mirror the screenshot: local AS 4216805503, router id 192.168.55.3, peer
// 192.168.55.1 / remote AS 4216825503.
// End-to-end regression for the reported bug: an existing /etc/frr/frr.conf
// (parapet-managed, with a neighbor whose session may be down) must import from
// the file alone — no dependency on vtysh/bgpd. Content is the operator's exact
// config, trailing spaces and all.
func TestImportBGPFromFRRFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/frr.conf"
	body := "! parapet-managed FRR configuration. Do not edit by hand. \n" +
		"frr defaults traditional \n! \n" +
		"router bgp 65001 \n" +
		" neighbor 10.1.1.1 remote-as 65003 \n" +
		" neighbor 10.1.1.1 password pass \n" +
		" neighbor 10.1.1.1 bfd \n" +
		" address-family ipv4 unicast \n" +
		"  network 10.1.1.0/24 \n" +
		"  network 10.1.2.0/24 \n" +
		"  neighbor 10.1.1.1 activate \n" +
		" exit-address-family \n!\n \n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := frrConfigPaths
	frrConfigPaths = []string{path}
	t.Cleanup(func() { frrConfigPaths = orig })
	// vtysh is absent in the test env (statFile seam), so this exercises the
	// file path with no daemon — exactly the failing scenario.
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })

	cfg, hasPw, ok := importBGPFromFRR()
	if !ok {
		t.Fatal("expected import to succeed from the config file alone")
	}
	if !cfg.Enabled || cfg.ASN != 65001 {
		t.Errorf("asn/enabled wrong: %+v", cfg)
	}
	if len(cfg.Neighbors) != 1 || cfg.Neighbors[0].Peer != "10.1.1.1" ||
		cfg.Neighbors[0].RemoteAS != 65003 || !cfg.Neighbors[0].BFD {
		t.Errorf("neighbor wrong: %+v", cfg.Neighbors)
	}
	if len(cfg.Networks) != 2 || cfg.Networks[0] != "10.1.1.0/24" || cfg.Networks[1] != "10.1.2.0/24" {
		t.Errorf("networks wrong: %+v", cfg.Networks)
	}
	if !hasPw {
		t.Error("expected password presence to be flagged")
	}
}

func TestSummaryToBGPConfig(t *testing.T) {
	sum := []byte(`{
	  "ipv4Unicast": {
	    "routerId": "192.168.55.3",
	    "as": 4216805503,
	    "peers": {
	      "192.168.55.1": {"remoteAs": 4216825503, "state": "Established", "peerUptimeMsec": 85911000, "pfxRcd": 17}
	    }
	  }
	}`)
	cfg, at, ok := summaryToBGPConfig(sum)
	if !ok {
		t.Fatal("expected ok for a running BGP speaker")
	}
	if !cfg.Enabled {
		t.Error("imported config should be enabled")
	}
	if cfg.ASN != 4216805503 {
		t.Errorf("ASN = %d, want 4216805503 (32-bit ASN must not truncate)", cfg.ASN)
	}
	if cfg.RouterID != "192.168.55.3" {
		t.Errorf("router-id = %q, want 192.168.55.3", cfg.RouterID)
	}
	if len(cfg.Neighbors) != 1 {
		t.Fatalf("got %d neighbors, want 1: %+v", len(cfg.Neighbors), cfg.Neighbors)
	}
	if cfg.Neighbors[0].Peer != "192.168.55.1" || cfg.Neighbors[0].RemoteAS != 4216825503 {
		t.Errorf("neighbor = %+v, want 192.168.55.1/4216825503", cfg.Neighbors[0])
	}
	if at["192.168.55.1"] != 0 {
		t.Errorf("index map wrong: %+v", at)
	}
}

// A dual-stack peer present in both address families must appear once, and a
// speaker with no peers yet (just a local AS) still imports.
func TestSummaryToBGPConfigDedupAndEmpty(t *testing.T) {
	dual := []byte(`{
	  "ipv4Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {"fd00::2": {"remoteAs": 65002, "state": "Established", "peerUptimeMsec": 1000}}},
	  "ipv6Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {"fd00::2": {"remoteAs": 65002, "state": "Established", "peerUptimeMsec": 1000}}}
	}`)
	cfg, _, ok := summaryToBGPConfig(dual)
	if !ok || len(cfg.Neighbors) != 1 {
		t.Errorf("dual-stack peer should dedup to 1 neighbor, got %d (ok=%v)", len(cfg.Neighbors), ok)
	}

	// Local AS, no peers → still a valid import (an enabled speaker).
	solo := []byte(`{"ipv4Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {}}}`)
	cfg2, _, ok2 := summaryToBGPConfig(solo)
	if !ok2 || cfg2.ASN != 65001 {
		t.Errorf("speaker with no peers should still import: ok=%v asn=%d", ok2, cfg2.ASN)
	}

	// Nothing at all → not ok.
	if _, _, ok3 := summaryToBGPConfig([]byte(`{}`)); ok3 {
		t.Error("empty summary should not import")
	}
}

func TestFRRTimersRenderAndParse(t *testing.T) {
	// Rendered when set; the 3:1 fast default the UI seeds new configs with.
	c := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001, KeepaliveTime: 4, HoldTime: 12})
	frrHas(t, c, " timers bgp 4 12\n")
	// Omitted entirely when unset (0/0) → FRR uses its own defaults.
	frrLacks(t, renderFRR(config.BGPConfig{Enabled: true, ASN: 65001}), "timers bgp")

	// Round-trips through the running-config parser.
	rc := "router bgp 65001\n timers bgp 4 12\n neighbor 10.0.0.2 remote-as 65002\nexit\n!\n"
	cfg, _, ok := parseRunningConfigBGP(rc)
	if !ok || cfg.KeepaliveTime != 4 || cfg.HoldTime != 12 {
		t.Errorf("timer parse: ok=%v ka=%d hold=%d, want 4/12", ok, cfg.KeepaliveTime, cfg.HoldTime)
	}
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
