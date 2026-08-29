package tui

import (
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// testSnapshot builds a snapshot with no I/O: a small config, a couple of
// live peers, and every capability present, so a test can exercise a page
// without a daemon, a config file, or FRR.
func testSnapshot() *snapshot {
	return &snapshot{
		cfgPath:  "/etc/gravinet/config.json",
		sockPath: "/run/gravinet/control.sock",
		version:  "1010",
		commit:   "test",
		cfg: &config.Config{
			NodeID:     "abcdef0123456789",
			Hostname:   "gn-test",
			LogLevel:   "info",
			UDPPorts:   []int{51820},
			TCPPorts:   []int{443},
			EnableIPv4: true,
			EnableIPv6: true,
			Networks: []config.Network{{
				ID: "00000000000000aa", Name: "corp", Enabled: true,
				Subnet4: "10.42.0.0/16", Address4: "10.42.0.1", MTU: 1380,
				Seeds: config.SeedList{{Address: "seed.example:51820"}},
				Routes: []config.Route{{CIDR: "192.168.5.0/24", Metric: 10, Enabled: true}},
			}},
		},
		peers: []livePeer{
			{net: "corp", PeerInfo: mesh.PeerInfo{NodeID: "1111111122222222", Hostname: "gn-peer", Overlay4: "10.42.0.2",
				Endpoint: "203.0.113.9:51820", Transport: "udp", RTTMs: 12.5, PathMTU: 1380}},
			{net: "corp", PeerInfo: mesh.PeerInfo{NodeID: "3333333344444444", Overlay4: "10.42.0.3", Relayed: true, RelayVia: "gn-peer"}},
		},
		ifaces: []mesh.IfaceInfo{{NetworkID: 0xaa, Name: "corp", Iface: "mesh0"}},
		caps:   caps{bgp: true, ipv6ra: true, dhcp: true, snmp: true, lldp: true, syslog: true},
	}
}

func testModel() *model {
	m := newModel(testSnapshot(), "dark", colorMono)
	m.w, m.h = 120, 40
	return m
}

// drawText renders the model and returns the screen as plain text.
func drawText(m *model, w, h int) string {
	s := NewScreen(w, h)
	m.draw(s)
	return s.String()
}

func TestOpensOnNetworksWithItsGroupExpanded(t *testing.T) {
	m := testModel()
	if m.section != "networks" {
		t.Errorf("opened on %q, want networks", m.section)
	}
	if m.expanded != "mesh" {
		t.Errorf("mesh group is not expanded, got %q", m.expanded)
	}
	out := drawText(m, 120, 40)
	if !strings.Contains(out, "[gravinet]") {
		t.Error("the brand is missing from the top bar")
	}
	if !strings.Contains(out, "gn-test") {
		t.Error("the node's identity is missing from the top bar")
	}
	if !strings.Contains(out, "corp") {
		t.Error("the configured network is not on the page")
	}
}

func TestRailIsAnAccordion(t *testing.T) {
	// Expanding one group collapses the others, matching ui.go's
	// expandOnlyRailGroup. Only the expanded group's pages are listed.
	m := testModel()
	entries := m.railEntries()
	var groups, items int
	for _, e := range entries {
		switch e.kind {
		case "group":
			groups++
		case "item":
			items++
			if e.group != "mesh" {
				t.Errorf("a page from the collapsed %q group is in the rail: %q", e.group, e.sec)
			}
		}
	}
	if groups != len(navGroups) {
		t.Errorf("rail shows %d group headers, want %d", groups, len(navGroups))
	}
	if items != 5 {
		t.Errorf("mesh has 5 pages, the rail lists %d", items)
	}
}

func TestRailHidesCapabilityGatedPages(t *testing.T) {
	// A page hidden in the browser must be unreachable here too — not merely
	// undrawn, but impossible to arrow onto.
	m := testModel()
	m.snap.caps = caps{} // a bare host: no FRR, no lldpd, no snmpd
	m.expanded = "traffic"
	for _, e := range m.railEntries() {
		if e.sec == "bgp" || e.sec == "ipv6ra" {
			t.Errorf("%q is in the rail on a host that cannot back it", e.sec)
		}
	}
	m.expanded = "monitor"
	for _, e := range m.railEntries() {
		if e.sec == "bgp-peers" || e.sec == "l2-peers" {
			t.Errorf("%q is in the rail on a host that cannot back it", e.sec)
		}
	}
	// And the ungated pages are all still there.
	found := false
	for _, e := range m.railEntries() {
		if e.sec == "metrics" {
			found = true
		}
	}
	if !found {
		t.Error("metrics vanished along with the gated pages")
	}
}

func TestEnterOnAGroupExpandsIt(t *testing.T) {
	m := testModel()
	m.railFocus = true
	// Walk to the traffic group header.
	for i, e := range m.railEntries() {
		if e.kind == "group" && e.group == "traffic" {
			m.railIdx = i
			break
		}
	}
	m.handleKey(key{t: keyEnter})
	if m.expanded != "traffic" {
		t.Fatalf("expanded = %q, want traffic", m.expanded)
	}
	// And the cursor stayed on the header it was on, which is now the row
	// above the pages that just appeared.
	e := m.railEntries()[m.railIdx]
	if e.kind != "group" || e.group != "traffic" {
		t.Errorf("cursor moved off the header it expanded: %+v", e)
	}
}

func TestEnterOnAPageOpensItAndReturnsFocus(t *testing.T) {
	m := testModel()
	m.railFocus = true
	for i, e := range m.railEntries() {
		if e.sec == "seeds" {
			m.railIdx = i
			break
		}
	}
	m.handleKey(key{t: keyEnter})
	if m.section != "seeds" {
		t.Fatalf("section = %q, want seeds", m.section)
	}
	if m.railFocus {
		t.Error("focus stayed on the rail after opening a page")
	}
	if !strings.Contains(drawText(m, 120, 40), "seed.example:51820") {
		t.Error("the seeds page did not render its data")
	}
}

func TestNavigatingResetsTheScroll(t *testing.T) {
	// Landing halfway down a page you have just navigated to is disorienting,
	// and the web admin does not do it either.
	m := testModel()
	m.scroll, m.hscroll = 12, 8
	m.setSection("bans")
	if m.scroll != 0 || m.hscroll != 0 {
		t.Errorf("scroll = %d,%d after navigating, want 0,0", m.scroll, m.hscroll)
	}
}

func TestTabMovesFocus(t *testing.T) {
	m := testModel()
	if !m.railFocus {
		t.Fatal("focus should start on the rail — see railFocus's own comment for why")
	}
	m.handleKey(key{t: keyTab})
	if m.railFocus {
		t.Error("tab did not move focus to the content pane")
	}
	m.handleKey(key{t: keyTab})
	if !m.railFocus {
		t.Error("tab did not move focus back to the rail")
	}
}

// TestArrowKeysNavigateImmediatelyOnOpen is a regression test for a reported
// bug: the console used to open with the content pane focused, and most
// pages fit inside the terminal with nothing to scroll — so the first
// several arrow-key presses somebody sent right after opening produced no
// visible reaction at all, which read as "up/down don't work for navigation"
// rather than as "there is nothing to scroll yet." The console now opens
// with the rail focused, so the very first press has to move the cursor.
func TestArrowKeysNavigateImmediatelyOnOpen(t *testing.T) {
	m := testModel()
	before := m.railIdx
	m.handleKey(key{t: keyDown})
	if m.railIdx == before {
		t.Fatal("the first Down press after opening did not move the rail cursor")
	}
	m.handleKey(key{t: keyUp})
	if m.railIdx != before {
		t.Error("Up did not move the cursor back")
	}
}

// TestRailFocusIsVisibleOnTheActivePage covers the other half of the same
// bug: even once the rail did have focus, landing the cursor back on the
// page already open (the state right after opening, or right after Enter)
// drew identically to not being focused at all — active-page styling took
// precedence over the focus highlight with no visual distinction between
// them. tabbing onto your own current page looked like nothing happened.
func TestRailFocusIsVisibleOnTheActivePage(t *testing.T) {
	m := testModel()
	m.railFocus = true
	// railIdx already sits on "networks", the open page, right after
	// opening — see syncRailIdx.
	// entries[0] is the "mesh" group header, drawn at y=2 (rail top);
	// entries[1] is "networks" itself, the open page, at y=3.
	s1 := NewScreen(120, 40)
	m.draw(s1)
	activeStyle := s1.StyleAt(1, 3)

	m.railFocus = false
	s2 := NewScreen(120, 40)
	m.draw(s2)
	unfocusedStyle := s2.StyleAt(1, 3)

	if activeStyle == unfocusedStyle {
		t.Error("the rail's cursor on the currently-open page draws identically whether or not the rail has focus")
	}
}

func TestArrowsMoveWhicheverPaneHasFocus(t *testing.T) {
	m := testModel()
	m.railFocus = false
	m.section = "readme" // a page long enough to scroll
	m.lazy.set("doc:readme", struct {
		lines []string
		path  string
	}{}, nil)
	before := m.railIdx
	m.handleKey(key{t: keyDown})
	if m.railIdx != before {
		t.Error("scrolling the page moved the rail cursor")
	}
	if m.scroll != 1 {
		t.Errorf("page scroll = %d, want 1", m.scroll)
	}

	m.railFocus = true
	m.scroll = 0
	m.handleKey(key{t: keyDown})
	if m.railIdx != before+1 {
		t.Errorf("rail cursor = %d, want %d", m.railIdx, before+1)
	}
	if m.scroll != 0 {
		t.Error("moving the rail cursor scrolled the page")
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []key{{t: keyRune, r: 'q'}, {t: keyCtrlC}, {t: keyCtrlD}} {
		m := testModel()
		if m.handleKey(k) {
			t.Errorf("%v did not quit", k)
		}
	}
}

func TestCtrlCQuitsFromTheSearchOverlayToo(t *testing.T) {
	// ISIG is cleared, so Ctrl-C is a key rather than a signal. Every mode
	// has to honour it or the console becomes unquittable.
	m := testModel()
	m.handleKey(key{t: keyRune, r: '/'})
	if !m.searching {
		t.Fatal("/ did not open the search overlay")
	}
	if m.handleKey(key{t: keyCtrlC}) {
		t.Error("ctrl-c did not quit from inside the search overlay")
	}
}

func TestThemeToggle(t *testing.T) {
	m := testModel()
	if m.theme != "dark" {
		t.Fatalf("theme = %q", m.theme)
	}
	m.handleKey(key{t: keyRune, r: 't'})
	if m.theme != "light" || m.pal.fg != lightPalette.fg {
		t.Errorf("theme = %q, palette fg = %v", m.theme, m.pal.fg)
	}
	m.handleKey(key{t: keyRune, r: 't'})
	if m.theme != "dark" || m.pal.fg != darkPalette.fg {
		t.Errorf("theme did not toggle back: %q", m.theme)
	}
}

func TestHelpPageListsEveryBinding(t *testing.T) {
	// helpKeys is the only description of the bindings that exists, so a
	// binding cannot be added without appearing on this page.
	m := testModel()
	m.handleKey(key{t: keyRune, r: '?'})
	if m.section != helpSection {
		t.Fatalf("? opened %q", m.section)
	}
	out := drawText(m, 120, 40)
	for _, b := range helpKeys {
		if !strings.Contains(out, b[0]) {
			t.Errorf("help page is missing the %q binding", b[0])
		}
	}
}

func TestSearchFindsAndOpensAPage(t *testing.T) {
	m := testModel()
	m.handleKey(key{t: keyRune, r: '/'})
	for _, r := range "syslog" {
		m.handleKey(key{t: keyRune, r: r})
	}
	if len(m.matches) == 0 || m.matches[0].sec != "syslog" {
		t.Fatalf("search for syslog gave %v", m.matches)
	}
	m.handleKey(key{t: keyEnter})
	if m.searching {
		t.Error("the overlay stayed open after Enter")
	}
	if m.section != "syslog" {
		t.Errorf("section = %q, want syslog", m.section)
	}
}

func TestSearchEscapeDoesNotNavigate(t *testing.T) {
	m := testModel()
	start := m.section
	m.handleKey(key{t: keyRune, r: '/'})
	for _, r := range "logs" {
		m.handleKey(key{t: keyRune, r: r})
	}
	m.handleKey(key{t: keyEsc})
	if m.searching {
		t.Error("escape did not close the overlay")
	}
	if m.section != start {
		t.Errorf("escape navigated to %q", m.section)
	}
}

func TestSearchRanksExactMatchesFirst(t *testing.T) {
	all := caps{bgp: true, ipv6ra: true, dhcp: true, snmp: true, lldp: true, syslog: true}
	hits := searchSections("dns", all)
	if len(hits) == 0 || hits[0].sec != "dns" {
		t.Fatalf("exact match did not rank first: %v", hits)
	}
	// And "dns-state" is also found, since n/N is meant to flip between them.
	found := false
	for _, h := range hits {
		if h.sec == "dns-state" {
			found = true
		}
	}
	if !found {
		t.Errorf("dns-state was not among the hits: %v", hits)
	}
}

func TestSearchSkipsGatedPages(t *testing.T) {
	if hits := searchSections("bgp", caps{}); len(hits) != 0 {
		t.Errorf("search offered a page the host cannot back: %v", hits)
	}
}

func TestSearchWithNoQueryListsEveryVisiblePage(t *testing.T) {
	all := caps{bgp: true, ipv6ra: true, dhcp: true, snmp: true, lldp: true, syslog: true}
	if got, want := len(searchSections("", all)), len(sectionKeys()); got != want {
		t.Errorf("empty query listed %d pages, want %d", got, want)
	}
}

func TestNextMatchWithNoSearchSaysSo(t *testing.T) {
	m := testModel()
	m.handleKey(key{t: keyRune, r: 'n'})
	if m.flash == "" {
		t.Error("n with no active search silently did nothing")
	}
}

func TestScrollIsClampedToThePage(t *testing.T) {
	// The page's length depends on the terminal's width, so the clamp lives
	// in the draw path, which is the only place that knows both numbers.
	m := testModel()
	m.scroll = 100000
	drawText(m, 120, 40)
	if m.scroll > 50 {
		t.Errorf("scroll was not clamped: %d", m.scroll)
	}
	m.scroll = -5
	drawText(m, 120, 40)
	if m.scroll != 0 {
		t.Errorf("negative scroll = %d", m.scroll)
	}
}

func TestDrawSurvivesTinyTerminals(t *testing.T) {
	m := testModel()
	for _, sz := range [][2]int{{1, 1}, {5, 3}, {20, 4}, {railWidth, 10}, {80, 24}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("draw panicked at %dx%d: %v", sz[0], sz[1], r)
				}
			}()
			drawText(m, sz[0], sz[1])
		}()
	}
}

func TestDrawEveryPageAtSeveralWidths(t *testing.T) {
	// The real point of this one: every page must render without panicking on
	// a snapshot with no daemon and no config, which is the state an operator
	// is most likely to open this console in.
	empty := &snapshot{cfgPath: "/nonexistent", sockPath: "/nonexistent",
		cfgErr: errTest, daemonErr: errTest, version: "1010", commit: "test",
		caps: caps{bgp: true, ipv6ra: true, dhcp: true, snmp: true, lldp: true, syslog: true}}

	for _, snap := range []*snapshot{testSnapshot(), empty} {
		for _, sec := range append(sectionKeys(), helpSection) {
			for _, w := range []int{60, 100, 200} {
				m := newModel(snap, "dark", colorMono)
				m.lazy.offline = true // no vtysh, no lldpcli, no disk
				m.section = sec
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s at width %d panicked: %v", sec, w, r)
						}
					}()
					out := drawText(m, w, 30)
					if !strings.Contains(out, "[gravinet]") {
						t.Errorf("%s at width %d lost the top bar", sec, w)
					}
				}()
			}
		}
	}
}

func TestUnreachableDaemonIsStatedNotImpliedByAnEmptyTable(t *testing.T) {
	// An empty table and a dead daemon look identical and mean opposite
	// things. Every live page has to say which it is.
	snap := testSnapshot()
	snap.daemonErr = errTest
	snap.peers, snap.bans, snap.routes, snap.ifaces = nil, nil, nil, nil

	for _, sec := range []string{"peers", "bans", "mesh-peers", "latency"} {
		m := newModel(snap, "dark", colorMono)
		m.section = sec
		out := drawText(m, 100, 30)
		if !strings.Contains(out, "not reachable") {
			t.Errorf("%s did not say the daemon is unreachable:\n%s", sec, out)
		}
	}
	// And the top bar says so too, so it is visible from any page.
	m := newModel(snap, "dark", colorMono)
	if !strings.Contains(drawText(m, 100, 30), "daemon down") {
		t.Error("the top bar does not report the daemon being down")
	}
}

func TestConfigPagesStillWorkWithoutTheDaemon(t *testing.T) {
	snap := testSnapshot()
	snap.daemonErr = errTest
	snap.ifaces = nil
	m := newModel(snap, "dark", colorMono)
	m.section = "networks"
	out := drawText(m, 120, 30)
	if !strings.Contains(out, "corp") {
		t.Error("the networks page lost its configured data when the daemon went away")
	}
	// But it must not claim the interface is up when it cannot know.
	if !strings.Contains(out, "?") {
		t.Error("the iface column should read ? when the daemon cannot be asked")
	}
}

func TestRefreshFallsBackWhenTheCurrentPageBecomesInvisible(t *testing.T) {
	// lldpd removed while the console is open would otherwise leave a page on
	// screen with no rail entry pointing at it — the same fallback
	// renderSection does.
	m := testModel()
	m.setSection("l2-peers")
	m.cfgPath, m.sockPath = "/nonexistent", "/nonexistent"
	m.snap.caps = caps{}
	if sectionVisible(m.section, m.caps()) {
		t.Fatal("test setup: l2-peers should now be invisible")
	}
	// refresh re-reads, which on these paths yields no config and no daemon;
	// what matters is the fallback, not the data.
	m.refresh()
	if m.section == "l2-peers" {
		t.Error("refresh left an invisible page on screen")
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "no such file or directory" }
