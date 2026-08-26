package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
)

// The role select records what an operator chose. Through v949 nothing
// recorded what the node was doing about it, so a node that had quietly
// stopped serving looked exactly like one that was serving fine — which is
// what "role just remembers the last one you choose" describes.
//
// Every case below leaves the stored mode saying one thing and the host doing
// another. None of them is an error the operator did anything wrong to reach.

func withFakeRelay(t *testing.T, f *fakeRelay) {
	t.Helper()
	prev := dhcpRelay
	dhcpRelay = f
	t.Cleanup(func() { dhcpRelay = prev })
}

// Off is the one mode where nothing running is the selected outcome, so there
// is no mismatch and nothing to explain.
func TestDHCPRuntimeOffReportsNothing(t *testing.T) {
	withFakeRelay(t, &fakeRelay{})
	r := dhcpRuntime(config.DHCPConfig{})
	if r.Role != "" || r.Why != "" {
		t.Errorf("a node doing nothing reported %+v, want silence", r)
	}
}

// Relay selected, links configured and running: the report names the links
// actually bound rather than the ones configured.
func TestDHCPRuntimeRelayReportsBoundLinks(t *testing.T) {
	c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{
		Links: []config.DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}}},
	}}
	f := &fakeRelay{}
	withFakeRelay(t, f)
	if _, err := applyDHCP(c); err != nil {
		t.Fatalf("apply: %v", err)
	}
	r := dhcpRuntime(c)
	if r.Role != "relay" {
		t.Fatalf("a running relay reported role %q", r.Role)
	}
	if len(r.Ifaces) != 1 || r.Ifaces[0] != "eth1" {
		t.Errorf("want the bound link named, got %v", r.Ifaces)
	}
	if r.Why != "" {
		t.Errorf("a relay doing what was asked explained itself anyway: %q", r.Why)
	}
}

// The relay half of the mismatch, in each of its shapes. The wording differs
// per case on purpose: the fix differs too — one needs a state tag toggled,
// one needs a server typed in, one needs a row added at all.
func TestDHCPRuntimeExplainsARelayThatIsNotRelaying(t *testing.T) {
	for name, tc := range map[string]struct {
		links []config.DHCPRelayLink
		want  string
	}{
		"no links at all": {nil, "no relay link is configured"},
		"every link parked": {
			[]config.DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}, Disabled: true}},
			"every relay link is disabled",
		},
		"enabled but no upstream": {
			[]config.DHCPRelayLink{{Iface: "eth1"}},
			"no enabled link has an upstream server",
		},
		"configured but nothing bound": {
			[]config.DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}}},
			"the relay is not listening on eth1",
		},
	} {
		// A relay that never started: the fake reports nothing listening.
		withFakeRelay(t, &fakeRelay{})
		c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{Links: tc.links}}
		r := dhcpRuntime(c)
		if r.Role != "" {
			t.Errorf("%s: reported a running relay (%q) when none is", name, r.Role)
		}
		if !strings.Contains(r.Why, tc.want) {
			t.Errorf("%s: explanation %q does not mention %q", name, r.Why, tc.want)
		}
	}
}

// Server selected with nothing servable. Kea refuses to start with no subnet4,
// so the apply stops it — and through v949 the page went on saying "server"
// with no indication the node had stopped.
func TestDHCPRuntimeExplainsAServerThatIsNotServing(t *testing.T) {
	withFakeRelay(t, &fakeRelay{})
	sub := func(iface string) config.DHCPSubnet {
		return config.DHCPSubnet{Iface: iface, Subnet: "10.1.1.0/24",
			PoolStart: "10.1.1.100", PoolEnd: "10.1.1.200", Router: "10.1.1.1"}
	}
	parked := sub("eth1")
	parked.Disabled = true

	for name, tc := range map[string]struct {
		subnets []config.DHCPSubnet
		want    string
	}{
		"none configured": {nil, "no subnet is configured"},
		"all disabled":    {[]config.DHCPSubnet{parked}, "every subnet is disabled"},
		// servableSubnets drops a subnet naming an interface this host does
		// not have, because Kea refuses the whole file for one it cannot find.
		// Dropping every subnet leaves server mode with nothing to serve.
		"interface absent": {[]config.DHCPSubnet{sub("definitely-not-a-nic")}, "does not have"},
	} {
		c := config.DHCPConfig{Mode: config.DHCPServer, Subnets: tc.subnets}
		r := dhcpRuntime(c)
		if r.Role != "" {
			t.Errorf("%s: reported a running server (%q) when none is", name, r.Role)
		}
		// Kea is not installed in this container, so that reason wins and is
		// itself correct — the assertion is that *some* reason is given and
		// that a reachable one is right.
		if r.Why == "" {
			t.Errorf("%s: a server that is not serving explained nothing", name)
			continue
		}
		if !keaInstalled() && !strings.Contains(r.Why, "not installed") {
			t.Errorf("%s: want the missing-Kea reason on a host without it, got %q", name, r.Why)
		}
		if keaInstalled() && !strings.Contains(r.Why, tc.want) {
			t.Errorf("%s: explanation %q does not mention %q", name, r.Why, tc.want)
		}
	}
}

// The report has to reach the page, or none of the above is visible.
func TestDHCPRuntimeIsServedAndRendered(t *testing.T) {
	if !strings.Contains(mustRead("dhcp_apply.go"), `"running":`) {
		t.Error("the GET no longer reports what DHCP is actually doing")
	}
	if !strings.Contains(indexHTML, "b.running") {
		t.Error("the page no longer reads the running state")
	}
	// Each card's pill must keep showing the configured state, not the running
	// one: it is how a role gets enabled before it can possibly be running, so
	// driving it from reality would make an unconfigured role impossible to
	// turn on. The running state goes beside the pill, never into it.
	for _, role := range []string{"server", "relay"} {
		call := between(t, indexHTML, "sectionCardHead('DHCP "+strings.ToUpper(role)+"'", "\n")
		if !strings.Contains(call, ", en,") {
			t.Errorf("the %s card's pill is not driven from the configured state: %s", role, call)
		}
		if strings.Contains(call, "run.") {
			t.Errorf("the %s card's pill is being driven from the running state: %s", role, call)
		}
	}
}

// The role dropdown is gone: two cards, each with the standard pill, and the
// exclusion coming from Mode being one field rather than from anything the
// page has to keep in step.
func TestDHCPUsesTwoCardsNotARolePicker(t *testing.T) {
	if strings.Contains(indexHTML, "dh-mode") {
		t.Error("the DHCP role dropdown is back")
	}
	for _, want := range []string{"sectionCardHead('DHCP SERVER'", "sectionCardHead('DHCP RELAY'"} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("missing %s — the two-card layout is not intact", want)
		}
	}
	// Both cards render on every load, whichever is enabled, so a
	// configuration that is not currently in service is still visible and
	// editable. Rendering only the enabled one is what made "off" look like
	// the configuration had been deleted.
	body := between(t, indexHTML, "function render(b){\n    const d = b.dhcp || {};", "\n  }")
	if strings.Contains(body, "if (mode === 'server') renderServer") {
		t.Error("the page renders only the selected role's card again")
	}
	for _, want := range []string{"renderServer(d, probs, mode === 'server'", "renderRelay(d, probs, mode === 'relay'"} {
		if !strings.Contains(body, want) {
			t.Errorf("render() no longer draws both cards unconditionally: missing %q", want)
		}
	}
}

// Adding a row must not enable the card it was added to. The pill is the
// control, as it is on every other card, and auto-enabling would flip a switch
// sitting visibly on the same card — or, with the other half running, have to
// choose between silently stopping it and silently doing nothing.
func TestDHCPAddingARowDoesNotEnableTheCard(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	add := between(t, src, `case "add", "update":`, `case "delete", "remove":`)
	if strings.Contains(add, "d.Mode = config.DHCPServer") {
		t.Error("adding a subnet silently enables the server card")
	}
	relayAdd := between(t, src, `case "relay-add", "relay-update":`, `case "relay-delete", "relay-remove":`)
	if strings.Contains(relayAdd, "d.Mode = config.DHCPRelay") {
		t.Error("adding a relay link silently enables the relay card")
	}
}

// The mutual exclusion was enforced at apply time against a service whose boot
// behaviour gravinet set and never unset, so it did not survive a reboot.
//
//	role = server, add a subnet   -> Kea started, and `systemctl enable`d
//	role = relay                  -> Kea stopped now; still enabled
//	reboot                        -> systemd starts Kea; gravinet starts the
//	                                 relay from the stored mode
//
// and the node comes back doing both at once on the same links — the exact
// state config.DHCPMode exists to make unrepresentable.
//
// Checked against the source, because the alternative is enabling and
// rebooting a real systemd unit on the machine running the tests.
func TestKeaIsDisabledWheneverItIsStopped(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	// Every teardown goes through the helper that does both.
	if !strings.Contains(src, "func keaStopAndDisable()") {
		t.Fatal("keaStopAndDisable is gone — the stop no longer survives a reboot")
	}
	body := between(t, src, "func keaStopAndDisable() {", "\n}")
	if !strings.Contains(body, `keaService("stop")`) || !strings.Contains(body, `keaService("disable")`) {
		t.Errorf("keaStopAndDisable must both stop and disable, got:%s", body)
	}
	// And nothing reintroduces a bare stop: a stop without a disable is the
	// bug, and it looks completely reasonable on the line it is written on.
	fn := between(t, src, "func applyDHCP(", "\n// handleDHCP")
	if strings.Contains(fn, `keaService("stop")`) {
		t.Error("applyDHCP stops Kea without disabling it, so the stop will not survive a reboot")
	}
	// The exclusion is re-asserted at daemon start, which is what heals a node
	// that was already left with an enabled unit before this existed.
	boot := between(t, src, "func StartDHCPRelay(", "\n}")
	if !strings.Contains(boot, "keaStopAndDisable()") {
		t.Error("daemon startup no longer re-asserts the role exclusion, so an already-affected node stays broken until someone re-saves the page")
	}
	if !strings.Contains(boot, "c.Mode != config.DHCPServer") {
		t.Error("startup teardown is not conditioned on the selected role")
	}
}

// If the two ever do overlap, the page has to say so rather than reporting the
// relay as healthy — which it is, and which is not the point.
func TestDHCPRuntimeReportsAServerRunningBesideTheRelay(t *testing.T) {
	src := mustRead("dhcp_runtime.go")
	if !strings.Contains(src, "keaActive()") {
		t.Fatal("the runtime report no longer asks whether Kea is running")
	}
	relay := between(t, src, "case config.DHCPRelay:", "// whyNotServing")
	if i := strings.Index(relay, "return dhcpRunning{Why: whyNotRelaying(c)}"); i < 0 {
		t.Fatal("relay branch not found")
	} else if !strings.Contains(relay[:i], "keaActive()") {
		t.Error("the relay branch reports the relay as healthy without first checking for a server running beside it")
	}
}

// NAT is node-global from v953, so its section must render on a node with no
// mesh networks. Guarding the shape, since the old per-network loop plus the
// "No networks." gate is exactly what made NAT unreachable on a LAN router.
func TestNATSectionIsNodeGlobal(t *testing.T) {
	sec := between(t, indexHTML, "function secNAT(c) {", "\n}")
	if strings.Contains(sec, "No networks.") {
		t.Error("the NAT section is gated on a mesh network existing again")
	}
	if strings.Contains(sec, "for (const cf of state.cfg)") {
		t.Error("the NAT section renders a card per mesh network again")
	}
	// The node-global switch moved from an <h3> inside the card to a pill
	// beside the page's own <h2> in v968, with the redundant "NAT" card
	// title dropped. What matters is unchanged: one switch for the node, not
	// one per network.
	if !strings.Contains(sec, "sectionTitlePill(c, en, '/api/nat'") {
		t.Error("the NAT section no longer puts its node-global switch on the page title")
	}
	if strings.Contains(sec, "sectionCardHead(") {
		t.Error("the NAT section still labels its card with the section name")
	}
	// The scope column is gone as of v966: where a rule applies is derived
	// from the rule, so a picker beside it was a second answer to a question
	// the rule already answers, and one that could contradict it.
	if strings.Contains(sec, "<th>scope</th>") {
		t.Error("the NAT table still renders a scope column")
	}
	if strings.Contains(sec, "nate-scope") || strings.Contains(sec, "natScopeOpts") {
		t.Error("the NAT editor still wires a scope picker")
	}
}

// QoS is node-global from v954, so its section must render on a node with no
// mesh networks — same guard as NAT's, and for the same reason.
func TestQoSSectionIsNodeGlobal(t *testing.T) {
	sec := between(t, indexHTML, "function secQoS(c) {", "\n}")
	if strings.Contains(sec, "No networks.") {
		t.Error("the QoS section is gated on a mesh network existing again")
	}
	if strings.Contains(sec, "for (const cf of state.cfg)") {
		t.Error("the QoS section renders a card per mesh network again")
	}
	if !strings.Contains(sec, "sectionTitlePill(c, en, '/api/qos'") {
		t.Error("the QoS section no longer puts its node-global switch on the page title")
	}
	if strings.Contains(sec, "sectionCardHead(") {
		t.Error("the QoS section still labels its card with the section name")
	}
	if !strings.Contains(sec, "<th>scope</th>") {
		t.Error("the QoS table has no scope column")
	}
	// Scope is part of a rule's key, so every op that addresses an existing
	// rule has to carry it or it will hit the wrong one.
	for _, op := range []string{"rule-enable", "delete"} {
		i := strings.Index(sec, op)
		if i < 0 {
			t.Errorf("op %q not found in the QoS section", op)
			continue
		}
		if !strings.Contains(sec[i:min(i+220, len(sec))], "scope") {
			t.Errorf("the %q op does not carry scope, so it can address the wrong rule", op)
		}
	}
}

// Shaping is keyed by interface from v960: one card, one row per entry, and
// no second card above it holding a node-wide default.
func TestShapingSectionIsPerInterface(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	if strings.Contains(sec, "No networks.") {
		t.Error("the shaping section is gated on a mesh network existing again")
	}
	if strings.Contains(sec, "netCardHead(cf") {
		t.Error("the shaping section renders a card per mesh network again")
	}
	// The node default card is gone, and with it the two-level model. Its
	// card head is the tell: it carried the node-wide enable toggle that
	// switched every network's limiter at once.
	if strings.Contains(sec, "sectionCardHead('BANDWIDTH'") {
		t.Error("the node-default bandwidth card is back, so shaping has two levels again")
	}
	// Every row addresses an interface. A row keyed by network would mean the
	// resolution step v960 removed had come back somewhere.
	if !strings.Contains(sec, "tr.dataset.iface") {
		t.Error("the rows no longer address an interface, so entries cannot be edited")
	}
	if strings.Contains(sec, "tr.dataset.net") {
		t.Error("the rows address a network again rather than the interface being shaped")
	}
	// There is no default to revert to any more, so - removes the entry.
	if strings.Contains(sec, "clear-override") {
		t.Error("the - button clears an override again, but nothing is inherited now")
	}
	if !strings.Contains(sec, "op:'delete'") {
		t.Error("- no longer deletes the shaping entry")
	}
	// With one flat list there is no total to mistake a rate for, so the
	// caveat the old model needed must not survive as dead prose.
	if strings.Contains(sec, "never shared between them") {
		t.Error("the card still warns about sharing, a caveat only the old two-level model needed")
	}
}

// + must be wired, or an interface can only ever be shaped by hand-editing
// the config: there is no per-network row appearing on its own any more.
func TestShapingCanAddAnInterface(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	if !strings.Contains(sec, "table._rowAdd") {
		t.Fatal("the shaping card has no + button, so no interface can be shaped")
	}
	add := between(t, indexHTML, "function bwAddRow(table, carries){", "\n}")
	if !strings.Contains(add, "op:'add'") {
		t.Error("the + row does not create a shaping entry")
	}
	// The picker is every interface on the host, from the shared inventory —
	// not just the devices gravinet runs a network on. Restricting to those
	// would make the picker a second place the enforcement boundary is
	// stated, and the weaker of the two: the carries cell says it per entry,
	// where it is actually load-bearing.
	if !strings.Contains(add, "systemInterfaces()") {
		t.Error("the + picker no longer reads the host interface inventory, so it cannot offer every interface")
	}
	// Configured mesh devices are unioned in even when absent from the host,
	// or a network that is not up yet could never be given a rate.
	if !strings.Contains(add, "for (const i of carries.keys()) all.add(i)") {
		t.Error("the + picker drops mesh interfaces that are not up, so a rate cannot be set before its network exists")
	}
	// Offering an interface is not a promise that a rate on it will bite.
	if !strings.Contains(add, "carries.has(iface)") {
		t.Error("adding an interface this node shapes nothing on no longer says so")
	}
}

// The firewall is node-global from v957: one card, no networks gate, and the
// rulebase read from config rather than from the live engine.
func TestFirewallSectionIsNodeGlobal(t *testing.T) {
	sec := between(t, indexHTML, "function secFirewall(c) {", "\n// fwScopeOpts")
	if strings.Contains(sec, "emptyCard(c, 'No networks.')") {
		t.Error("the firewall rules tab is gated on a mesh network existing again")
	}
	if strings.Contains(sec, "netCardHead(cf") {
		t.Error("the firewall renders a card per mesh network again")
	}
	if !strings.Contains(sec, "sectionTitlePill(c, en, '/api/firewall'") {
		t.Error("the firewall section no longer puts its node-global switch on the page title")
	}
	if strings.Contains(sec, "sectionCardHead(") {
		t.Error("the firewall section still labels its card with the section name")
	}
	if !strings.Contains(sec, "<th>scope</th>") {
		t.Error("the firewall table has no scope column")
	}
	// Every rule op addresses the node list, never a network. A lingering
	// net: would send the edit down the pre-v957 engine path.
	if strings.Contains(sec, "net:cf.id") {
		t.Error("a firewall op still addresses a single network")
	}
}

// Editing a rule updates it in place rather than deleting and re-adding.
// The old delete+add lost the rule's id, its position in the ordered list —
// which decides what matches first — and its hit counters.
func TestFirewallEditIsInPlace(t *testing.T) {
	fn := between(t, indexHTML, "function startFwEdit(tr){", "\nfunction ")
	if !strings.Contains(fn, "op:'update'") {
		t.Error("the firewall row editor no longer updates in place")
	}
	if strings.Contains(fn, "op:'del'") {
		t.Error("the firewall row editor deletes and re-adds again, losing the rule's id, position and counters")
	}
}

// The handler must not send rule edits to the engine: config is the source of
// truth from v957, and the engine path is the one that needs a mesh network.
func TestFirewallHandlerIsConfigFirst(t *testing.T) {
	src := mustRead("webadmin.go")
	h := between(t, src, "func (s *Server) handleFirewall(", "\nfunc fwRuleToConfig")
	for _, engineOp := range []string{"s.be.FirewallAdd(", "s.be.FirewallDelete(", "s.be.FirewallMove("} {
		if strings.Contains(h, engineOp) {
			t.Errorf("handleFirewall still calls %s; rule edits go to config from v957", engineOp)
		}
	}
	// Counters are live traffic rather than configuration, so that one op does
	// still reach the engine.
	if !strings.Contains(h, "s.be.FirewallResetCounters(") {
		t.Error("reset-counters no longer reaches the engine, where the tallies actually live")
	}
}

// The rate cells have to look editable.
//
// They shipped in v956 as bare <td class="bw-cell"> — a class with no styling
// at all — while the node default above used .bw-edit, which carries the
// dotted underline and hover colour that say "double-click me". The cells were
// wired up and worked; they just looked like dead text. v960 removed the node
// default, so .bw-edit on the rows is now the only affordance on the page and
// losing it would leave nothing at all.
func TestShapingRateCellsLookEditable(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	rows := between(t, sec, "for (const e of entries)", "if (!entries.length)")
	for _, dir := range []string{`data-dir="up"`, `data-dir="down"`} {
		i := strings.Index(rows, dir)
		if i < 0 {
			t.Fatalf("no %s cell in the shaping rows", dir)
		}
		cell := rows[i:min(i+220, len(rows))]
		if !strings.Contains(cell, "bw-edit") {
			t.Errorf("the %s cell does not carry .bw-edit, so it has no affordance and reads as dead text: %s", dir, cell)
		}
	}
}

// An empty table must say what to do rather than render as a blank card: with
// no node default and no rows appearing on their own, a fresh node's Shaping
// page is empty by default and + is the only way forward.
func TestShapingEmptyStateSaysWhatToDo(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	if !strings.Contains(sec, "No interface is shaped") {
		t.Error("an unshaped node gets a blank table with nothing telling it about +")
	}
}

// The card's how-to prose belongs behind the help toggle, like every other
// sub-card description. The pre-v960 version ("Double-click a network's up or
// down rate to give it its own limit...") was a plain .hint and so was always
// on screen; v959 had even made it unconditional, which was right for a page
// whose table looked inert but is no longer needed now the empty state and the
// + button carry that job.
//
// The two other pieces of text on the card must NOT follow it into hiding.
// Both are status rather than explanation: with help off, a card that showed a
// bare empty table, or a row that looked enforced when it is not, would be
// worse than the paragraph ever was.
func TestShapingProseIsBehindHelpButStatusIsNot(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	i := strings.Index(sec, "Use + to shape any interface")
	if i < 0 {
		t.Fatal("the shaping card no longer says how to use it")
	}
	open := strings.LastIndex(sec[:i], `class="`)
	if open < 0 || !strings.Contains(sec[open:i], "help-desc") {
		t.Error("the shaping how-to is not .help-desc, so it shows whether or not help is on")
	}
	for _, status := range []string{"unavailable here", "No interface is shaped"} {
		j := strings.Index(sec, status)
		if j < 0 {
			t.Fatalf("%q is gone", status)
		}
		o := strings.LastIndex(sec[:j], `class="`)
		if o >= 0 && strings.Contains(sec[o:j], "help-desc") {
			t.Errorf("%q was hidden behind help; it is status, not an explanation", status)
		}
	}
}

// The row must say which machinery enforces it, and must say when nothing
// can. Shaping a physical NIC means programming a qdisc, which is Linux-only
// and needs iproute2; on a host without either, the rate is saved and not
// applied, and that is exactly the silent-success shape this page has spent
// four releases removing.
func TestShapingRowSaysHowItIsEnforced(t *testing.T) {
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	if !strings.Contains(sec, "state.shapingKinds") {
		t.Error("the row no longer distinguishes tunnel-shaped from kernel-shaped entries")
	}
	if !strings.Contains(sec, "state.shapingKernel") {
		t.Error("the row no longer checks whether this host can program a qdisc")
	}
	if !strings.Contains(sec, "unavailable here") {
		t.Error("a kernel-shaped entry on a host that cannot program tc renders as if it were in force")
	}
}
