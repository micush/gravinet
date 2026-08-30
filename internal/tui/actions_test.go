package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

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
	if legend := m.actionLegendSegments(); len(legend) != 0 {
		t.Errorf("legend for a not-mine ban should be empty, got %v", legend)
	}

	m.snap.bans = []liveBan{{net: "corp", BanInfo: mesh.BanInfo{Target: "mine", Mine: true}}}
	m.selID = "corp" + idSep + "mine"
	if legend := m.actionLegendSegments(); len(legend) == 0 {
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
	// Peers' 'e' opens "notes", and 'e' occurs inside that word (not just as
	// a first letter) — footerKeySegment merges it there rather than
	// showing a separate "e" token, so the footer reads "notes", not
	// "e notes". 'd' bans, and 'd' does not occur anywhere in "ban", so
	// that one keeps its separate, underlined token.
	for _, want := range []string{"notes", "space", "d ban"} {
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

// TestFooterKeySegmentMergesWhenKeyMatchesLabel confirms the fold: when the
// key is a single letter and the label starts with the same letter, the
// separate key token disappears and the label's own first letter carries
// the underline instead — "n"+"next" becomes "next" with 'n' underlined,
// not "n next".
func TestFooterKeySegmentMergesWhenKeyMatchesLabel(t *testing.T) {
	seg := footerKeySegment("n", "next")
	if seg.text != "next" {
		t.Errorf("text = %q, want %q", seg.text, "next")
	}
	if seg.ulPos != 0 {
		t.Errorf("ulPos = %d, want 0 (the n in next)", seg.ulPos)
	}
}

// TestFooterKeySegmentKeepsKeySeparateWhenItDoesNotMatch confirms the other
// half: a row action's key is fixed by convention, not derived from its
// label, and the two frequently disagree (Peers' 'e' opens "notes"). The key
// must still be shown, accurately, as its own token — never silently
// dropped or (worse) implying the label's own first letter is the trigger
// when it isn't.
// TestFooterKeySegmentMergesAnywhereInTheLabelNotJustTheFirstLetter is the
// direct regression test for the reported bug: Networks' 'E' action opens
// "advanced", and the old version only checked the label's own first
// letter ('a'), so 'E' had nowhere to merge into and was shown as a bare,
// unmarked capital letter floating in front of the word — "E advanced"
// with nothing visually connecting the two. "advanc(e)d" already contains
// the letter; the fix is to search the whole label, not just position 0.
func TestFooterKeySegmentMergesAnywhereInTheLabelNotJustTheFirstLetter(t *testing.T) {
	seg := footerKeySegment("E", "advanced")
	if seg.text != "advanced" {
		t.Errorf("text = %q, want %q (no separate key token)", seg.text, "advanced")
	}
	if seg.ulPos != 6 { // a-d-v-a-n-c-e-d: the e is at index 6
		t.Errorf("ulPos = %d, want 6 (the e inside advanced)", seg.ulPos)
	}
}

// TestFooterKeySegmentKeepsKeySeparateWhenItDoesNotMatchAnywhere confirms
// the true fallback still works: a row action's key is fixed by convention
// and occasionally doesn't appear anywhere in its own label at all (Peers'
// 'd' bans, and there is no 'd' anywhere in "ban"). That case still needs
// its own leading token — and, the other half of the same reported bug,
// that token must itself be underlined, never left as a bare, unmarked
// character next to the word ("? help" and "/ search" were being built
// this way, with no underline position at all).
func TestFooterKeySegmentKeepsKeySeparateWhenItDoesNotMatchAnywhere(t *testing.T) {
	seg := footerKeySegment("d", "ban")
	if seg.text != "d ban" {
		t.Errorf("text = %q, want %q", seg.text, "d ban")
	}
	if seg.ulPos != 0 {
		t.Errorf("ulPos = %d, want 0 (the d itself, underlined — not left bare)", seg.ulPos)
	}
}

// TestFooterKeySegmentUnderlinesSymbolKeysToo covers '?' and '/': neither is
// a letter, so neither can merge into its label, but the fallback token
// must still carry an underline rather than sit there unmarked — SGR
// underline renders on punctuation exactly as it does on a letter.
func TestFooterKeySegmentUnderlinesSymbolKeysToo(t *testing.T) {
	for _, tc := range []struct{ key, label, want string }{
		{"?", "help", "? help"},
		{"/", "search", "/ search"},
	} {
		seg := footerKeySegment(tc.key, tc.label)
		if seg.text != tc.want {
			t.Errorf("footerKeySegment(%q, %q).text = %q, want %q", tc.key, tc.label, seg.text, tc.want)
		}
		if seg.ulPos != 0 {
			t.Errorf("footerKeySegment(%q, %q).ulPos = %d, want 0 — a symbol key must still be underlined",
				tc.key, tc.label, seg.ulPos)
		}
	}
}

// TestFooterKeySegmentNamedKeyNeverFolds confirms a multi-rune key display
// (tab, enter, space) never tries to fold into its label — there's no
// single rune to compare, so it always shows as "key label" with the key's
// own first letter underlined, correctly labeling the physical key rather
// than implying a letter to type.
func TestFooterKeySegmentNamedKeyNeverFolds(t *testing.T) {
	seg := footerKeySegment("tab", "rail/page")
	if seg.text != "tab rail/page" {
		t.Errorf("text = %q, want %q", seg.text, "tab rail/page")
	}
	if seg.ulPos != 0 {
		t.Errorf("ulPos = %d, want 0 (the t in tab)", seg.ulPos)
	}
}

// TestDrawFooterUsesPipeSeparatorsAndUnderlinesRealTriggers is an end-to-end
// check of the actual rendered footer: pipes between segments, and the
// underline landing on the correct rune for both a merged segment (next)
// and a key-fixed-by-convention segment (Peers' "e notes").
func TestDrawFooterUsesPipeSeparatorsAndUnderlinesRealTriggers(t *testing.T) {
	m := testModel()
	m.setSection("peers")
	m.railFocus = false

	s := NewScreen(160, 40)
	m.draw(s)
	footerY := m.h - 1
	footer := s.Row(footerY)
	if !strings.Contains(footer, "\u2502") {
		t.Errorf("footer has no pipe separators: %q", footer)
	}

	// strings.Index returns a *byte* offset; the footer holds multi-byte
	// runes before either target (the │ separators, the ↑↓ arrows), so that
	// offset has to be converted to a rune count — a screen column — before
	// it means anything to StyleAt, which indexes cells by column.
	col := func(substr string) int {
		i := strings.Index(footer, substr)
		if i < 0 {
			return -1
		}
		return utf8.RuneCountInString(footer[:i])
	}

	// Find "next" in the rendered footer and confirm its 'n' — not some
	// other rune in the row — is the one carrying the underline.
	idx := col("next")
	if idx < 0 {
		t.Fatalf("footer missing \"next\": %q", footer)
	}
	if got := s.StyleAt(idx, footerY); !got.underline {
		t.Errorf("the n in \"next\" is not underlined, style = %+v", got)
	}

	// Peers' 'e' opens "notes" — 'e' occurs inside that word (index 3), so
	// it merges rather than showing a separate "e" token. The underline
	// must land on that 'e', not on any of notes' other letters.
	nidx := col("notes")
	if nidx < 0 {
		t.Fatalf("footer missing \"notes\": %q", footer)
	}
	if got := s.StyleAt(nidx+3, footerY); !got.underline { // n-o-t-(e)-s
		t.Errorf("the e inside \"notes\" is not underlined, style = %+v", got)
	}
	if got := s.StyleAt(nidx, footerY); got.underline { // the leading 'n'
		t.Error("\"notes\" should not be underlined at its own first letter — the trigger is the e inside it")
	}
}

// TestNetworksFooterMergesEIntoAdvanced is a direct regression test, on the
// real render, for the reported bug: Networks' 'E' action opens "advanced",
// and used to show as a bare "E" floating in front of the word with no
// underline connecting the two, because the old matching only checked a
// label's first letter. The footer must instead show plain "advanced" with
// the 'e' already inside it underlined.
func TestNetworksFooterMergesEIntoAdvanced(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.railFocus = false

	s := NewScreen(160, 40)
	m.draw(s)
	footerY := m.h - 1
	footer := s.Row(footerY)
	if strings.Contains(footer, "E advanced") {
		t.Fatalf("the old, broken form is still present: %q", footer)
	}
	idx := strings.Index(footer, "advanced")
	if idx < 0 {
		t.Fatalf("footer missing \"advanced\": %q", footer)
	}
	col := utf8.RuneCountInString(footer[:idx])
	if got := s.StyleAt(col+6, footerY); !got.underline { // a-d-v-a-n-c-(e)-d
		t.Errorf("the e inside \"advanced\" is not underlined, style = %+v", got)
	}
}
