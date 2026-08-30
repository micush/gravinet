package tui

import (
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// pageText builds one page and renders it to plain text at a fixed width.
func pageText(t *testing.T, sec string, snap *snapshot) string {
	t.Helper()
	l := newLazyState()
	l.offline = true
	cards := buildPage(sec, pageCtx{snap: snap, lazy: l})
	return linesText(layout(cards, testCtx(100)))
}

func TestNetworksPairsConfigWithLiveInterface(t *testing.T) {
	// The iface column is the join between what the node is told to do and
	// what it is doing, and it answers "why can I not reach anything on this
	// network" more often than the rest of the page together.
	snap := testSnapshot()
	out := pageText(t, "networks", snap)
	if !strings.Contains(out, "mesh0") {
		t.Errorf("live interface missing:\n%s", out)
	}

	// Enabled in the file with no interface is a network that failed to come
	// up, and must not read the same as a disabled one.
	snap.ifaces = nil
	out = pageText(t, "networks", snap)
	if !strings.Contains(out, "not up") {
		t.Errorf("an enabled network with no interface should say so:\n%s", out)
	}
}

func TestKeysPageNeverPrintsKeyMaterial(t *testing.T) {
	// This screen ends up in a scrollback buffer, and frequently in somebody's
	// terminal recording.
	const secret = "c2VjcmV0LWtleS1tYXRlcmlhbC1kby1ub3QtcHJpbnQ="
	snap := testSnapshot()
	snap.cfg.Networks[0].Keys[0] = config.KeySlot{Key: secret, Label: "current", Enabled: true}

	out := pageText(t, "keys", snap)
	if strings.Contains(out, secret) {
		t.Fatalf("the keys page printed key material:\n%s", out)
	}
	if !strings.Contains(out, "current") {
		t.Errorf("the slot's label should still be shown:\n%s", out)
	}
	if !strings.Contains(out, "(set)") {
		t.Errorf("a populated slot should be marked as populated:\n%s", out)
	}
}

func TestSNMPPageNeverPrintsCommunityStrings(t *testing.T) {
	const community = "s3cr3t-community"
	snap := testSnapshot()
	snap.cfg.SNMP = config.SNMPConfig{
		Enabled:     true,
		Communities: []config.SNMPCommunity{{Community: community}},
	}
	if out := pageText(t, "snmp", snap); strings.Contains(out, community) {
		t.Fatalf("the snmp page printed a community string:\n%s", out)
	}
}

func TestFirewallRendersNegationExplicitly(t *testing.T) {
	// An inverted source drawn without its "!" is a rule that means the
	// opposite of what it says, which is the worst thing a firewall page can
	// get wrong.
	snap := testSnapshot()
	// Firewall rules are live control-socket state (confirmed against
	// cmd/gravinet's cmdFW, which sends every verb through control.Do, not
	// openCfg) — pageFirewall reads snap.firewall, not the config file.
	snap.firewall = []mesh.FirewallRule{
		{ID: 1, Action: "allow", Src: "10.0.0.0/8", Dst: "10.42.0.0/16", Scope: "corp"},
		{ID: 2, Action: "deny", Src: "10.0.0.0/8", SrcNegate: true, Scope: "corp"},
	}
	out := pageText(t, "firewall", snap)
	if !strings.Contains(out, "!10.0.0") {
		t.Errorf("a negated source lost its marker:\n%s", out)
	}
	// An empty field means "any", and must say so rather than being blank —
	// a blank cell in a firewall table is ambiguous.
	if !strings.Contains(out, "any") {
		t.Errorf("an unset field should read as any:\n%s", out)
	}
}

func TestFirewallDisabledRuleIsMarked(t *testing.T) {
	snap := testSnapshot()
	snap.firewall = []mesh.FirewallRule{
		{ID: 1, Action: "allow", Scope: "corp", Disabled: true},
	}
	if out := pageText(t, "firewall", snap); !strings.Contains(out, "off") {
		t.Errorf("a disabled rule is not marked as such:\n%s", out)
	}
}

func TestRoutesShowsAdvertisedAndLearnedSeparately(t *testing.T) {
	// These are different lists — what this node offers versus what has
	// actually reached it — and blending them hides the case where a peer's
	// route is not arriving.
	snap := testSnapshot()
	snap.routes = []liveRoute{{net: "corp", RouteInfo: mesh.RouteInfo{CIDR: "172.16.0.0/12", Via: "gn-peer", Metric: 5}}}
	out := pageText(t, "routes", snap)
	if !strings.Contains(out, "192.168.5.0/24") {
		t.Errorf("advertised route missing:\n%s", out)
	}
	if !strings.Contains(out, "172.16.0.0/12") {
		t.Errorf("learned route missing:\n%s", out)
	}
	if !strings.Contains(out, "ADVERTISED") || !strings.Contains(out, "LEARNED") {
		t.Errorf("the two lists are not separately labelled:\n%s", out)
	}
}

func TestRelayedPeerDoesNotShowAZeroEndpoint(t *testing.T) {
	// A relayed session has no direct underlay address of its own; rendering
	// the zero AddrPort is how a working relay gets mistaken for a broken
	// endpoint.
	out := pageText(t, "peers", testSnapshot())
	if strings.Contains(out, "0.0.0.0:0") || strings.Contains(out, "invalid AddrPort") {
		t.Errorf("a relayed peer rendered a zero endpoint:\n%s", out)
	}
	if !strings.Contains(out, "relay") {
		t.Errorf("the relayed peer is not marked as relayed:\n%s", out)
	}
}

func TestPagesWithNoDataSayWhichKindOfNothingItIs(t *testing.T) {
	// "no seeds configured" and "the daemon is not reachable" are different
	// findings; a page must never render the second as the first.
	snap := testSnapshot()
	snap.cfg.Networks[0].Seeds = nil
	if out := pageText(t, "seeds", snap); !strings.Contains(out, "no seeds configured") {
		t.Errorf("empty seeds list:\n%s", out)
	}
}

func TestSpeedtestAndCaptureStateTheirGaps(t *testing.T) {
	// Both are absent for stated reasons that match the CLI's. A page that
	// merely rendered blank would read as broken.
	snap := testSnapshot()
	if out := pageText(t, "speedtest", snap); !strings.Contains(out, "control-socket") {
		t.Errorf("the speedtest page does not explain why it is absent:\n%s", out)
	}
	out := pageText(t, "capture", snap)
	if !strings.Contains(out, "gravinet monitor capture mesh0") {
		t.Errorf("the capture page should name the command for each live interface:\n%s", out)
	}
}

func TestSettingsCoversTheGearPage(t *testing.T) {
	out := pageText(t, "settings", testSnapshot())
	for _, want := range []string{
		"udp ports", "tcp ports", "keepalive", "peer timeout", "log level",
		"managed", "manager", "worker threads", "udp gso", "ip forwarding",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("settings is missing the %q row:\n%s", want, out)
		}
	}
	// And it names the two rows that deliberately have nothing behind them
	// here, matching cmd/gravinet/navparity_test.go's own noCLIForm set.
	if !strings.Contains(out, "Dark mode") || !strings.Contains(out, "TLS certificate") {
		t.Errorf("settings does not account for the two rows with no equivalent:\n%s", out)
	}
}

func TestSettingsMarksATransportBeingOff(t *testing.T) {
	// An empty port list is "this transport is off", which is a deliberate
	// configuration and not a missing value — and worth colouring, because a
	// node with both off cannot peer at all.
	snap := testSnapshot()
	snap.cfg.TCPPorts = nil
	if out := pageText(t, "settings", snap); !strings.Contains(out, "off") {
		t.Errorf("a disabled transport should read as off:\n%s", out)
	}
}

func TestAboutReportsTheRunningBuild(t *testing.T) {
	out := pageText(t, "about", testSnapshot())
	for _, want := range []string{"1010", "test", "gn-test"} {
		if !strings.Contains(out, want) {
			t.Errorf("about is missing %q:\n%s", want, out)
		}
	}
}

func TestLogItemsColourByLevel(t *testing.T) {
	// Detection is a narrow match on the level tag: a line whose *message*
	// contains the word error is not an error line, and colouring it red is
	// how a log page ends up looking alarming when nothing is wrong.
	if got := logTone("2026-01-01 [error] tun write failed"); got != "danger" {
		t.Errorf("an error line got tone %q", got)
	}
	if got := logTone("2026-01-01 [warn] pmtu shrank"); got != "warn" {
		t.Errorf("a warn line got tone %q", got)
	}
	if got := logTone("2026-01-01 [info] handshake error rate is nominal"); got != "" {
		t.Errorf("an info line mentioning errors got tone %q", got)
	}
	// Consecutive same-level lines become one run, so the block reads as one
	// block rather than as one item per line.
	items := logItems([]string{"[info] a", "[info] b", "[error] c"})
	if len(items) != 2 {
		t.Errorf("expected two runs, got %d", len(items))
	}
}

func TestFormattersMatchTheCLI(t *testing.T) {
	// formatRate and formatUptime are transcribed from cmd/gravinet so the
	// metrics page and "gravinet monitor metrics" report identically.
	for _, c := range []struct {
		in   float64
		want string
	}{{0, "0.0 B/s"}, {1023, "1023.0 B/s"}, {1024, "1.0 KB/s"}, {1 << 20, "1.0 MB/s"}} {
		if got := formatRate(c.in); got != c.want {
			t.Errorf("formatRate(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   uint64
		want string
	}{{59, "0m"}, {3600, "1h 0m"}, {90000, "1d 1h 0m"}} {
		if got := formatUptime(c.in); got != c.want {
			t.Errorf("formatUptime(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDashMarksAbsentValues(t *testing.T) {
	// A blank cell must always mean "no value" and never "the renderer
	// dropped it".
	if got := dash(""); got != "\u2014" {
		t.Errorf("dash(\"\") = %q", got)
	}
	if got := dash("   "); got != "\u2014" {
		t.Errorf("dash(whitespace) = %q", got)
	}
	if got := dash("x"); got != "x" {
		t.Errorf("dash(x) = %q", got)
	}
}

// Note: this package used to carry TestEditHintsNameRealCommands and
// TestReadOnlyPagesCarryNoEditHint here, checking that every page's "edit"
// footer named a real CLI command. editHint itself is gone now — every page
// that used to show one has a real action reaching the same command
// instead (see sections.go's note on its removal) — and the coverage those
// two tests gave is now provided far more precisely by the per-action argv
// tests in actions_mesh_test.go, actions_traffic_test.go,
// actions_naming_test.go, actions_settings_test.go, and
// actions_system_test.go, each of which drives a real action through the
// real dispatch path and asserts on the exact argv produced, rather than
// regex-matching a footer string.
