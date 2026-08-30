package tui

import (
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
	"gravinet/internal/service"
)

func TestDispatchAddOpensTheRegisteredForm(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.dispatchAdd()
	if m.form == nil {
		t.Fatal("'a' on networks should have opened the add form")
	}
	if m.form.spec.title != "add network" {
		t.Errorf("wrong form opened: %q", m.form.spec.title)
	}
}

func TestDispatchAddOnASectionWithNoAddFormSaysSo(t *testing.T) {
	m := testModel()
	m.setSection("about") // a read-only page with no registered actions at all
	m.dispatchAdd()
	if m.form != nil {
		t.Fatal("a form opened on a page with nothing to add")
	}
	if m.flash == "" {
		t.Error("dispatchAdd was silent instead of explaining why nothing happened")
	}
}

func TestDispatchRowActionRunsAConfirmForDelete(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	// The test snapshot's one network, "corp", is what syncSelection lands
	// the cursor on by default.
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("'d' on a network should ask for confirmation before deleting")
	}
}

func TestDispatchRowActionWithNoSelectionSaysSo(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.selTable, m.selID = "", ""
	m.dispatchRowAction('d')
	if m.confirm != nil || m.form != nil {
		t.Error("an action ran with nothing selected")
	}
	if m.flash == "" {
		t.Error("dispatchRowAction was silent instead of explaining why nothing happened")
	}
}

func TestDispatchRowActionForAnUnregisteredKeySaysSo(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.dispatchRowAction('z')
	if m.flash == "" {
		t.Error("an unbound row-action key should explain nothing happened, not do nothing silently")
	}
}

func TestActionLegendReflectsTheSelectedRowNotAStaticList(t *testing.T) {
	// Bans only offers 'd' on a row this node itself issued (see
	// bansActions' row func) — the legend for a not-mine ban must not
	// advertise a delete that would just fail.
	m := testModel()
	m.snap.bans = []liveBan{{net: "corp", BanInfo: mesh.BanInfo{Target: "notmine", Mine: false}}}
	m.setSection("bans")
	m.selTable, m.selID = "bans", "corp"+idSep+"notmine"
	if legend := m.actionLegend(); legend != "" {
		t.Errorf("legend for a not-mine ban should be empty, got %q", legend)
	}

	m.snap.bans = []liveBan{{net: "corp", BanInfo: mesh.BanInfo{Target: "mine", Mine: true}}}
	m.selID = "corp" + idSep + "mine"
	if legend := m.actionLegend(); legend == "" {
		t.Error("legend for this node's own ban should offer delete")
	}
}

// TestEveryRegisteredRowActionHasALabel is a regression test for a real bug:
// every rowAction built with only {edit: ...} or {confirm: ..., run: ...}
// (i.e. every one of them except the hand-written toggles) was missing its
// label field, so the footer rendered a bare key with nothing after it —
// "e   d   ·  tab rail/page ..." — on every single page with row actions.
// dispatchRowAction and the confirm dialog both still worked (they don't
// read label at all), so nothing here failed until a human looked at the
// footer, which is exactly the kind of bug a plain "does the action run"
// test cannot catch. This walks every row action actually reachable on a
// representative, populated snapshot for each group and asserts none of
// them render blank.
func TestEveryRegisteredRowActionHasALabel(t *testing.T) {
	check := func(t *testing.T, m *model, sections []string) {
		t.Helper()
		for _, sec := range sections {
			set, ok := sectionActions[sec]
			if !ok || set.row == nil {
				continue
			}
			m.setSection(sec)
			for _, row := range m.currentRows() {
				for k, act := range set.row(m, row) {
					if act.label == "" {
						t.Errorf("%s: row %+v key %q has no label — the footer would render it blank",
							sec, row, string(k))
					}
				}
			}
		}
	}

	t.Run("mesh", func(t *testing.T) {
		m := testModel()
		// Populate a key slot and a peer so keys/peers have real rows too,
		// not just the ones testSnapshot() already sets up for networks/
		// seeds/bans.
		m.snap.cfg.Networks[0].Keys[0] = config.KeySlot{Key: "somekey", Label: "current", Enabled: true}
		check(t, m, []string{"networks", "keys", "seeds", "peers", "bans"})
	})

	t.Run("traffic", func(t *testing.T) {
		m := newModel(trafficTestSnapshot(), "dark", colorMono)
		m.w, m.h = 120, 40
		check(t, m, []string{"nat", "qos", "bandwidth", "routes", "bgp", "firewall", "ipv6ra"})
	})

	t.Run("naming", func(t *testing.T) {
		m := newModel(namingTestSnapshot(), "dark", colorMono)
		m.w, m.h = 120, 40
		check(t, m, []string{"dns", "hosts"})
	})

	t.Run("system", func(t *testing.T) {
		m := newModel(testSnapshot(), "dark", colorMono)
		m.w, m.h = 120, 40
		m.snap.cfg.Discovery.Interfaces = []config.DiscoveryIface{{Name: "eth0", LLDP: true, CDP: true}}
		m.lazy.set("syslog", service.SyslogInfo{Targets: []service.SyslogTarget{{Remote: "logs.example", Port: 514, Protocol: "udp"}}}, nil)
		m.lazy.set("sys-users", service.UsersInfo{Users: []service.SysUser{{Name: "alice", Exists: true}}}, nil)
		m.lazy.set("config-history", []config.SnapshotMeta{{ID: "snap1"}}, nil)
		check(t, m, []string{"lldp", "syslog", "users", "config-history"})
	})
}

// TestPeersFooterMatchesTheReportedBug is a direct regression test for the
// bug as it was actually seen: the Peers page's footer rendered "e   space
// disable  d   ·  tab rail/page ..." — blank after 'e' and 'd' — because
// their rowActions had no label. This drives the real footer text (not just
// actionLegend in isolation) and checks the exact shape that was wrong.
func TestPeersFooterMatchesTheReportedBug(t *testing.T) {
	m := testModel()
	m.setSection("peers")
	m.railFocus = false
	out := drawText(m, 120, 40)
	footer := lastNonBlankLine(out)
	if strings.Contains(footer, "e   space") || strings.Contains(footer, "d   \u00b7") {
		t.Errorf("footer still has a blank action label: %q", footer)
	}
	for _, want := range []string{"e notes", "space", "d ban"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q: %q", want, footer)
		}
	}
}

func lastNonBlankLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}
