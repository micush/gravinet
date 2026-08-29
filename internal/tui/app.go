package tui

// The model, and the loop that drives it.
//
// The web admin's shell is a top bar, a 188px rail, and a content pane, and
// this is that in characters: a header row, a rail column of the same
// collapsible accordion groups, a content pane of cards, and a footer of key
// bindings that the browser does not need because it has a mouse.
//
// Everything that decides what appears on screen lives on model and is driven
// by two methods — handleKey and draw — neither of which touches a file
// descriptor. run() is the only part that does, and it is thirty lines. That
// is deliberate: app_test.go builds a model, feeds it keys, and reads the
// Screen back as text, which is a far better test than anything that needs a
// pty to exist.

import (
	"os"
	"strings"
	"time"
)

// railWidth is the rail's fixed width, chosen the way ui.go's 188px was: wide
// enough for the longest label at its indent ("config-history" under a
// two-space bullet) with a little air, narrow enough to leave the content
// pane usable on an 80-column terminal.
const railWidth = 22

// model is the whole console's state.
type model struct {
	// Layout.
	w, h int

	// Navigation.
	section  string
	expanded string // the one expanded rail group; the accordion, as in ui.go
	// railFocus is which pane the arrow keys and j/k drive: the rail's
	// cursor when true, the page's scroll when false. Starts true.
	//
	// It matters which one starts true. A page frequently fits inside the
	// terminal with nothing to scroll — most cards on most pages do — so a
	// console that opened with the content pane focused could sit there
	// producing no visible reaction at all to the first several arrow-key
	// presses somebody sent it, which reads as the keys not working rather
	// than as "there is nothing here to scroll yet." Starting on the rail
	// means the first press does something visible on every page, which is
	// also the ordinary convention for a keyboard-driven menu (a file
	// manager, htop, less's own search list) — you can move before you have
	// chosen to look at anything in particular.
	railFocus bool
	railIdx   int // cursor position within the rail's visible entries

	// Content scrolling. hscroll only ever moves on pages with mono items,
	// which are the ones whose columns matter and so are truncated rather
	// than wrapped; see content.go's mono.
	scroll  int
	hscroll int

	// Row selection, for pages with at least one selectable table (see
	// content.go's table.selectKey and rows.go). selTable/selID identify the
	// selected row by its stable id rather than by position, the same way
	// the rail identifies a section by key rather than by index — so the
	// cursor survives a refresh reordering the list under it. Both empty
	// means nothing is selected, which is also the state on every page with
	// no selectable rows; selSection records which page they belong to, so
	// leftover selection state from a different page can never bleed
	// through after navigating away and back.
	selSection string
	selTable   string
	selID      string

	// The three mutually-exclusive modal states an editing action can put
	// the console into — see forms.go. At most one is non-nil at a time.
	form    *formState
	confirm *confirmState
	result  *resultState

	// Data.
	snap *snapshot
	lazy *lazyState

	// Presentation.
	pal   palette
	cm    colorMode
	theme string

	// The search overlay, mirroring the top bar's global search.
	searching bool
	query     string
	matches   []searchHit
	matchIdx  int

	// A transient message in the footer: what the last key did, or why it
	// did nothing. Cleared on the next keystroke, so it never becomes stale
	// furniture.
	flash string

	// Paths, for the refresh path to reload with.
	cfgPath, sockPath string
	version, commit   string

	quit bool
}

// newModel builds the initial state. Split from newApp so tests can construct
// a model without an Options or a terminal.
func newModel(snap *snapshot, theme string, cm colorMode) *model {
	m := &model{
		w: 100, h: 30,
		section:   defaultSection,
		expanded:  groupFor(defaultSection),
		railFocus: true, // see the field's own comment for why this matters
		snap:      snap,
		lazy:      newLazyState(),
		theme:     theme,
		pal:       paletteFor(theme),
		cm:        cm,
	}
	if snap != nil {
		m.cfgPath, m.sockPath = snap.cfgPath, snap.sockPath
		m.version, m.commit = snap.version, snap.commit
	}
	m.syncRailIdx()
	m.syncSelection()
	return m
}

// ---- the rail -----------------------------------------------------------

// railEntry is one drawable line in the rail: a group header, a page under
// it, or one of the two pinned entries in the foot.
type railEntry struct {
	kind  string // "group", "item", "foot"
	group string
	sec   string
	text  string
}

// railEntries builds what the rail currently shows: every group header, the
// pages of the expanded group only (the accordion behaviour ui.go's
// expandOnlyRailGroup implements), and the foot. Capability-gated pages are
// filtered here, so a hidden page cannot be reached by arrowing onto it —
// the same guarantee sectionVisible gives the browser.
func (m *model) railEntries() []railEntry {
	var out []railEntry
	caps := m.caps()
	for _, g := range navGroups {
		out = append(out, railEntry{kind: "group", group: g.name, text: g.name})
		if g.name != m.expanded {
			continue
		}
		for _, it := range g.items {
			if !sectionVisible(it.key, caps) {
				continue
			}
			out = append(out, railEntry{kind: "item", group: g.name, sec: it.key, text: label(it.key)})
		}
	}
	return append(out, railEntry{kind: "foot", sec: settingsSection, text: "Settings"})
}

func (m *model) caps() caps {
	if m.snap == nil {
		return caps{}
	}
	return m.snap.caps
}

// syncRailIdx puts the rail cursor on the active section, expanding its group
// if needed. Called whenever the section changes by any route — a rail click,
// a search hit, the fallback when a gated section becomes invisible.
func (m *model) syncRailIdx() {
	if g := groupFor(m.section); g != "" {
		m.expanded = g
	}
	for i, e := range m.railEntries() {
		if e.sec != "" && e.sec == m.section {
			m.railIdx = i
			return
		}
	}
	m.railIdx = 0
}

// setSection moves to a page, resetting scroll — landing halfway down a page
// you have just navigated to is disorienting, and the web admin does not do
// it either.
func (m *model) setSection(sec string) {
	if sec == m.section {
		return
	}
	m.section = sec
	m.scroll, m.hscroll = 0, 0
	m.syncRailIdx()
	m.syncSelection()
}

// currentCards builds the page currently on screen. Called fresh wherever
// it's needed (row navigation, action dispatch, drawing) rather than cached
// on the model — buildPage is a pure, cheap function of the snapshot and the
// lazy cache (see sections.go's own doc comment), and caching it would mean
// invalidating the cache on every one of the several things that can change
// what it returns, which is more bookkeeping than the function costs to
// just call again.
func (m *model) currentCards() []card {
	return buildPage(m.section, pageCtx{snap: m.snap, lazy: m.lazy})
}

// currentRows is the current page's selectable rows, in document order.
func (m *model) currentRows() []selRow {
	return flattenSelectable(m.currentCards())
}

// syncSelection puts the row cursor on a sensible row after the page or its
// data changed: the first selectable row if none was chosen yet or the page
// changed, or — the common case, a refresh — wherever the previously
// selected id still is, or the first row if that id is gone. The one thing
// it never does is leave a stale (selTable, selID) pointing at nothing:
// every action dispatch and every draw assumes that if selID is non-empty,
// findSelRow will find it.
func (m *model) syncSelection() {
	rows := m.currentRows()
	if len(rows) == 0 {
		m.selSection, m.selTable, m.selID = m.section, "", ""
		return
	}
	if m.selSection == m.section {
		if _, ok := findSelRow(rows, m.selTable, m.selID); ok {
			return // still there; leave the cursor exactly where it was
		}
	}
	m.selSection, m.selTable, m.selID = m.section, rows[0].tableKey, rows[0].id
}

// moveSelection steps the row cursor by delta positions through the current
// page's flattened row list, clamped rather than wrapping — running off
// either end of a list and reappearing at the other is the kind of surprise
// that makes a cursor feel unreliable.
func (m *model) moveSelection(delta int) {
	rows := m.currentRows()
	if len(rows) == 0 {
		return
	}
	i, ok := findSelRow(rows, m.selTable, m.selID)
	if !ok {
		i = 0
	}
	i += delta
	if i < 0 {
		i = 0
	}
	if i >= len(rows) {
		i = len(rows) - 1
	}
	m.selTable, m.selID = rows[i].tableKey, rows[i].id
}

// selectedRow returns the row the cursor is currently on, if any.
func (m *model) selectedRow() (selRow, bool) {
	rows := m.currentRows()
	i, ok := findSelRow(rows, m.selTable, m.selID)
	if !ok {
		return selRow{}, false
	}
	return rows[i], true
}

// ---- drawing ------------------------------------------------------------

// draw paints the whole console into s. Every frame is drawn from scratch
// into a fresh Screen; the diffing that keeps this cheap happens one layer
// down, in Screen.render.
func (m *model) draw(s *Screen) {
	m.w, m.h = s.Size()
	base := style{}.withFg(m.pal.fg)
	s.Fill(0, 0, m.w, m.h, ' ', base)

	m.drawTop(s)
	m.drawRail(s)
	m.drawContent(s)
	m.drawFooter(s)
	if m.searching {
		m.drawSearch(s)
	}
	if m.modalOpen() {
		m.drawModal(s)
	}
}

// drawTop is the header: the brand, then the node's identity, then a live
// indicator for the daemon. The web admin's top bar carries the brand, the
// search box and the peer picker; the search box here is an overlay on a key
// and the peer picker has no terminal equivalent (see the package comment),
// so what is left is the brand and the answer to "which node am I looking
// at", which is the question the peer picker exists to answer.
func (m *model) drawTop(s *Screen) {
	bar := style{}.withFg(m.pal.fg)
	s.Fill(0, 0, m.w, 1, ' ', bar)
	x := s.Print(1, 0, "[gravinet]", style{}.withFg(m.pal.fg).withBold())

	if m.snap != nil {
		host := ""
		if m.snap.cfg != nil && m.snap.cfg.Hostname != "" {
			host = m.snap.cfg.Hostname
		} else if h, err := os.Hostname(); err == nil {
			host = h
		}
		if host != "" {
			x = s.Print(x+2, 0, host, style{}.withFg(m.pal.mut))
		}
		x = s.Print(x+2, 0, "v"+m.snap.version, style{}.withFg(m.pal.mut))

		state, st := "daemon up", style{}.withFg(m.pal.ok)
		if !m.snap.daemonUp() {
			state, st = "daemon down", style{}.withFg(m.pal.danger)
		}
		if n := m.w - 1 - len(state); n > x+1 {
			s.Print(n, 0, state, st)
		}
	}
	// The rule under the header, the terminal's --line border-bottom.
	s.Fill(0, 1, m.w, 1, boxH, style{}.withFg(m.pal.line))
}

// drawRail paints the accordion. The active page is drawn in reverse video
// against the accent, which is what ui.go's `.rail-tab.active { background:
// var(--acc); color:#fff }` amounts to with a character cell; the rail
// cursor, when the rail has focus and is sitting somewhere other than the
// active page, gets the hover background instead.
func (m *model) drawRail(s *Screen) {
	top, bottom := 2, m.h-2
	sidebar := style{}.withBg(m.pal.sidebar)
	s.Fill(0, top, railWidth, bottom-top, ' ', sidebar)
	s.Fill(railWidth, top, 1, bottom-top, boxV, style{}.withFg(m.pal.line).withBg(m.pal.sidebar))

	entries := m.railEntries()
	// Scroll the rail only when it does not fit, keeping the cursor visible.
	visible := bottom - top - 2 // two rows reserved for the foot and its rule
	start := 0
	if len(entries) > visible && m.railIdx >= visible {
		start = m.railIdx - visible + 1
	}

	y := top
	for i := start; i < len(entries) && y < bottom-2; i++ {
		e := entries[i]
		if e.kind == "foot" {
			break
		}
		var st style
		text := e.text
		switch e.kind {
		case "group":
			// The chevron mirrors ui.go's .rail-chevron, rotated when the
			// group is collapsed.
			chev := "\u25b8"
			if e.group == m.expanded {
				chev = "\u25be"
			}
			text = chev + " " + strings.ToUpper(e.text)
			st = style{}.withFg(m.pal.mut).withBg(m.pal.sidebar)
		default:
			text = "   " + e.text
			st = style{}.withFg(m.pal.mut).withBg(m.pal.sidebar)
		}
		active := e.sec != "" && e.sec == m.section
		focused := m.railFocus && i == m.railIdx
		switch {
		case active && focused:
			// Still the accent fill — it is still the open page — but
			// underlined too, so the rail's keyboard cursor is visible even
			// when it lands on the one row that was already highlighted for
			// an unrelated reason (being the page on screen). Without this,
			// tabbing into the rail on the page you're already viewing — the
			// common case right after opening the console, or right after
			// following a search hit — produced no visible change at all,
			// which read as the rail (and then the arrow keys) not working.
			st = style{}.withFg(m.pal.panel).withBg(m.pal.acc).withBold().withUnderline()
		case active:
			st = style{}.withFg(m.pal.panel).withBg(m.pal.acc).withBold()
		case focused:
			st = style{}.withFg(m.pal.fg).withBg(m.pal.hover)
		}
		s.PrintPad(0, y, railWidth, " "+text, st)
		y++
	}

	// The foot: a divider, then Settings, pinned regardless of scroll — the
	// same arrangement .rail-foot has.
	fy := bottom - 2
	s.Fill(0, fy, railWidth, 1, boxH, style{}.withFg(m.pal.line).withBg(m.pal.sidebar))
	st := style{}.withFg(m.pal.mut).withBg(m.pal.sidebar)
	activeFoot := m.section == settingsSection
	focusedFoot := m.railFocus && m.railIdx == len(entries)-1
	switch {
	case activeFoot && focusedFoot:
		st = style{}.withFg(m.pal.panel).withBg(m.pal.acc).withBold().withUnderline()
	case activeFoot:
		st = style{}.withFg(m.pal.panel).withBg(m.pal.acc).withBold()
	case focusedFoot:
		st = style{}.withFg(m.pal.fg).withBg(m.pal.hover)
	}
	s.PrintPad(0, fy+1, railWidth, " SETTINGS", st)
}

// contentLines lays out the current page. Recomputed per frame: at these
// sizes the layout is microseconds, and caching it would mean invalidating
// the cache on a resize, a theme change, a refresh, and a lazy value
// arriving, which is four chances to show a stale screen in exchange for
// nothing measurable.
func (m *model) contentLines() []line {
	inner := m.contentWidth()
	cards := buildPage(m.section, pageCtx{snap: m.snap, lazy: m.lazy})
	head := m.headingLines(inner)
	return append(head, layout(cards, layoutCtx{pal: m.pal, width: inner})...)
}

func (m *model) contentWidth() int {
	w := m.w - railWidth - 3
	if w < 20 {
		w = 20
	}
	return w
}

// headingLines is the page's h2 and the description under it — ui.go's
// `<h2 class="sec">` plus secHelp. The description is the rail tooltip, which
// in a browser is hidden until hover and here is simply shown: a terminal has
// no hover, and the line is the most useful thing on a page somebody has
// arrowed onto without knowing what it is.
func (m *model) headingLines(w int) []line {
	out := []line{
		{{sectionHeading(m.section), style{}.withFg(m.pal.fg).withBold()}},
	}
	if d := descFor(m.section); d != "" {
		for _, l := range wrap(d, w) {
			out = append(out, line{{l, style{}.withFg(m.pal.mut)}})
		}
	}
	return append(out, line{})
}

// drawContent paints the page, clipped to the pane and offset by the scroll.
func (m *model) drawContent(s *Screen) {
	top, bottom := 2, m.h-2
	x0 := railWidth + 2
	lines := m.contentLines()

	// Clamp the scroll here rather than in the key handler: the page's length
	// depends on the terminal's width, so a scroll that was valid before a
	// resize may not be after one, and this is the only place that knows
	// both numbers.
	maxScroll := len(lines) - (bottom - top)
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}

	for i, y := m.scroll, top; i < len(lines) && y < bottom; i, y = i+1, y+1 {
		x := x0
		for _, sp := range lines[i] {
			text := sp.text
			if m.hscroll > 0 {
				// Horizontal scrolling applies to the line as a whole, so a
				// span that is entirely to the left of the offset disappears
				// and the one straddling it is cut.
				r := []rune(text)
				if len(r) <= m.hscroll {
					continue
				}
				text = string(r[m.hscroll:])
			}
			x = s.Print(x, y, text, sp.st)
			if x >= m.w {
				break
			}
		}
	}

	// A scroll indicator, drawn only when the page actually overflows.
	if maxScroll > 0 {
		pct := 100 * m.scroll / maxScroll
		tag := itoa(pct) + "%"
		s.Print(m.w-len(tag)-1, bottom-1, tag, style{}.withFg(m.pal.mut).withDim())
	}
}

// drawFooter is the key bar. A terminal has no affordances of its own, so the
// bindings have to be on screen; this is the one piece of furniture with no
// counterpart in the browser.
func (m *model) drawFooter(s *Screen) {
	y := m.h - 1
	s.Fill(0, y, m.w, 1, ' ', style{}.withFg(m.pal.mut))
	if m.flash != "" {
		s.Print(1, y, truncate(m.flash, m.w-2), style{}.withFg(m.pal.warn))
		return
	}
	keys := "tab rail/page  \u2191\u2193 move  enter open  / search  n next  r refresh  t theme  ? help  q quit"
	if _, ok := sectionActions[m.section]; ok {
		var parts []string
		if sectionActions[m.section].add != nil {
			parts = append(parts, "a add")
		}
		if legend := m.actionLegend(); legend != "" {
			parts = append(parts, legend)
		}
		if len(parts) > 0 {
			keys = joinLegend(parts) + "  \u00b7  " + keys
		}
	}
	s.Print(1, y, truncate(keys, m.w-2), style{}.withFg(m.pal.mut))
}

// joinLegend joins the per-page action hints with the same two-space
// separator the static key list uses, so the footer reads as one continuous
// legend rather than two differently-spaced halves.
func joinLegend(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "  "
		}
		out += p
	}
	return out
}

// drawSearch paints the search overlay: an input line and the matches under
// it, standing in for the top bar's dropdown.
func (m *model) drawSearch(s *Screen) {
	w := min(60, m.w-4)
	x, y := (m.w-w)/2, 3
	h := min(len(m.matches), 8) + 2

	panel := style{}.withBg(m.pal.panel).withFg(m.pal.fg)
	s.Fill(x, y, w, h, ' ', panel)
	s.Fill(x, y, w, 1, ' ', panel)
	s.Print(x+1, y, truncate("search: "+m.query+"\u2588", w-2), panel)
	s.Fill(x, y+1, w, 1, boxH, style{}.withFg(m.pal.line).withBg(m.pal.panel))

	if len(m.matches) == 0 {
		s.PrintPad(x+1, y+2, w-2, "no matching page", style{}.withFg(m.pal.mut).withBg(m.pal.panel))
		return
	}
	for i := 0; i < len(m.matches) && i < 8; i++ {
		hit := m.matches[i]
		st := style{}.withFg(m.pal.fg).withBg(m.pal.panel)
		if i == m.matchIdx {
			st = style{}.withFg(m.pal.panel).withBg(m.pal.acc)
		}
		text := label(hit.sec)
		if g := groupFor(hit.sec); g != "" {
			text = g + " \u203a " + text
		}
		s.PrintPad(x+1, y+2+i, w-2, text, st)
	}
}

// ---- key handling -------------------------------------------------------

// handleKey applies one keystroke. Returns false when the console should
// exit. Every binding here is also listed in the help page, and helpKeys is
// the single source both read from, so a binding cannot exist without being
// documented.
func (m *model) handleKey(k key) bool {
	m.flash = ""
	if m.modalOpen() {
		return m.handleModalKey(k)
	}
	if m.searching {
		return m.handleSearchKey(k)
	}

	switch k.t {
	case keyCtrlC, keyCtrlD:
		return false
	case keyTab:
		m.railFocus = !m.railFocus
		return true
	case keyShiftTab:
		m.railFocus = !m.railFocus
		return true
	case keyCtrlL:
		// Conventional "repaint": the model has nothing to fix, but a
		// terminal that has been written over by another process does, and
		// the caller forces a full redraw when this returns.
		return true
	case keyUp:
		m.moveUp(1, true)
		return true
	case keyDown:
		m.moveDown(1, true)
		return true
	case keyPgUp:
		m.moveUp(m.pageStep(), false)
		return true
	case keyPgDn:
		m.moveDown(m.pageStep(), false)
		return true
	case keyHome:
		m.jumpTop()
		return true
	case keyEnd:
		m.jumpBottom()
		return true
	case keyLeft:
		if m.hscroll > 0 {
			m.hscroll -= 4
			if m.hscroll < 0 {
				m.hscroll = 0
			}
		} else {
			m.railFocus = true
		}
		return true
	case keyRight:
		if m.railFocus {
			m.railFocus = false
		} else {
			m.hscroll += 4
		}
		return true
	case keyEnter:
		m.activate()
		return true
	case keyEsc:
		m.railFocus = false
		return true
	case keyRune:
		return m.handleRune(k.r)
	}
	return true
}

func (m *model) handleRune(r rune) bool {
	switch r {
	case 'q':
		return false
	case 'j':
		m.moveDown(1, true)
	case 'k':
		m.moveUp(1, true)
	case 'g':
		m.jumpTop()
	case 'G':
		m.jumpBottom()
	case '/':
		m.searching = true
		m.query = ""
		m.matches = searchSections("", m.caps())
		m.matchIdx = 0
	case 'n':
		m.nextMatch(1)
	case 'N':
		m.nextMatch(-1)
	case 'r':
		m.refresh()
	case 't':
		m.toggleTheme()
	case '?':
		m.setSection(helpSection)
	case 'a':
		m.dispatchAdd()
	case 'e':
		m.dispatchRowAction('e')
	case 'd':
		m.dispatchRowAction('d')
	case ' ':
		if !m.railFocus && len(m.currentRows()) > 0 {
			m.dispatchRowAction(' ')
		} else {
			m.moveDown(m.pageStep(), false)
		}
	default:
		return true
	}
	return true
}

// jumpTop/jumpBottom are g/G and Home/End's shared destination logic: jump
// the row cursor to the first/last selectable row when the content pane is
// row-navigable, or jump the raw scroll position otherwise — the same split
// moveUp/moveDown draws between Up/Down and PgUp/PgDn, for the same reason.
func (m *model) jumpTop() {
	if m.railFocus {
		m.railIdx = 0
		return
	}
	if rows := m.currentRows(); len(rows) > 0 {
		m.selTable, m.selID = rows[0].tableKey, rows[0].id
		m.ensureSelectionVisible()
		return
	}
	m.scroll = 0
}

func (m *model) jumpBottom() {
	if m.railFocus {
		return
	}
	if rows := m.currentRows(); len(rows) > 0 {
		last := rows[len(rows)-1]
		m.selTable, m.selID = last.tableKey, last.id
		m.ensureSelectionVisible()
		return
	}
	m.scroll = 1 << 30
}

// moveUp/moveDown move whichever pane has focus. One pair of methods rather
// than two, so a new binding cannot accidentally scroll the content while the
// rail has focus.
//
// rowNav distinguishes "this keystroke means move the cursor" (Up/Down, j/k)
// from "this keystroke means scroll the viewport" (PgUp/PgDn): on a page
// with selectable rows, the two are different things on purpose. Up/Down
// moves the row cursor one row at a time and the viewport follows it just
// far enough to keep it visible (see ensureSelectionVisible); PgUp/PgDn
// pages the raw text regardless of the cursor, which is still how to look
// through a table with more rows than fit on screen without moving the
// cursor through every one of them first.
func (m *model) moveUp(n int, rowNav bool) {
	if m.railFocus {
		for i := 0; i < n && m.railIdx > 0; i++ {
			m.railIdx--
		}
		return
	}
	if rowNav && len(m.currentRows()) > 0 {
		m.moveSelection(-n)
		m.ensureSelectionVisible()
		return
	}
	m.scroll -= n
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m *model) moveDown(n int, rowNav bool) {
	if m.railFocus {
		entries := m.railEntries()
		for i := 0; i < n && m.railIdx < len(entries)-1; i++ {
			m.railIdx++
		}
		return
	}
	if rowNav && len(m.currentRows()) > 0 {
		m.moveSelection(n)
		m.ensureSelectionVisible()
		return
	}
	m.scroll += n // clamped in drawContent, which knows the page length
}

// ensureSelectionVisible scrolls just enough to bring the selected row back
// into the viewport, the way moving a cursor in any list widget does. It
// locates the row's exact line via rowLineOffset — the same real-layout
// re-invocation rows_test.go cross-checks against actual rendered output —
// rather than a separate estimate of where it must be.
func (m *model) ensureSelectionVisible() {
	row, ok := m.selectedRow()
	if !ok {
		return
	}
	cards := m.currentCards()
	w := m.contentWidth()
	head := len(m.headingLines(w))
	offset := head + rowLineOffset(cards, layoutCtx{pal: m.pal, width: w}, row)

	top, bottom := 2, m.h-2
	visible := bottom - top
	if visible <= 0 {
		return
	}
	switch {
	case offset < m.scroll:
		m.scroll = offset
	case offset >= m.scroll+visible:
		m.scroll = offset - visible + 1
	}
}

func (m *model) pageStep() int {
	if n := m.h - 5; n > 1 {
		return n
	}
	return 1
}

// activate is Enter: on a group header it expands that group, on a page it
// opens it. The accordion behaviour matches ui.go's — expanding one group
// collapses the others.
func (m *model) activate() {
	if !m.railFocus {
		return
	}
	entries := m.railEntries()
	if m.railIdx < 0 || m.railIdx >= len(entries) {
		return
	}
	e := entries[m.railIdx]
	switch e.kind {
	case "group":
		if m.expanded == e.group {
			m.expanded = ""
		} else {
			m.expanded = e.group
		}
		// The cursor stays on the header it was on, which after an expand is
		// the row above the pages that just appeared.
		for i, en := range m.railEntries() {
			if en.kind == "group" && en.group == e.group {
				m.railIdx = i
				break
			}
		}
	default:
		m.setSection(e.sec)
		m.railFocus = false
	}
}

// refresh re-reads everything: the config file, the daemon, and every cached
// lazy value. This is what the r key is for, and it is also what makes the
// console usable while something is being changed from another window.
func (m *model) refresh() {
	m.snap = loadSnapshot(m.cfgPath, m.sockPath, m.version, m.commit)
	m.lazy.invalidateAll()
	// A page whose section became invisible while this was open — lldpd
	// removed, FRR uninstalled — would otherwise stay on screen with no rail
	// entry pointing at it. Same fallback renderSection does.
	if !sectionVisible(m.section, m.caps()) {
		m.setSection(defaultSection)
		return
	}
	// The data just changed underneath whatever row was selected — a rule
	// added elsewhere, a peer that reconnected and got a new position in the
	// list. syncSelection re-finds the same id if it's still there, or falls
	// back to the first row rather than leaving selID pointing at nothing.
	m.syncSelection()
	m.flash = "refreshed"
}

// toggleTheme switches palettes, the terminal counterpart of the Settings
// page's Dark mode row — which is the one setting there that this console
// does have, precisely because it is a per-client preference rather than
// something in config.json.
func (m *model) toggleTheme() {
	if m.theme == "light" {
		m.theme = "dark"
	} else {
		m.theme = "light"
	}
	m.pal = paletteFor(m.theme)
	m.flash = "theme: " + m.theme
}

// ---- the loop -----------------------------------------------------------

// app is the model plus the things that talk to the terminal.
type app struct {
	m        *model
	out      *os.File
	prev     *Screen
	interval time.Duration
}

func newApp(opts Options, out *os.File) *app {
	theme := opts.Theme
	if theme != "dark" && theme != "light" {
		theme = detectTheme()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	// The first gather happens before the first frame, so the console opens
	// showing the node rather than opening blank and filling in. It is a
	// config read and four unix-socket dials; the slow reads are all behind
	// lazy.go and do not run until a page asks for one.
	snap := loadSnapshot(opts.ConfigPath, opts.ControlSocket, opts.Version, opts.Commit)
	return &app{
		m:        newModel(snap, theme, parseColorMode(opts.Color)),
		out:      out,
		interval: interval,
	}
}

// run is the event loop. Three sources feed it: keys, the poll tick, and a
// lazy fetch completing. Keys arrive on a goroutine because the read is
// blocking and there is no portable way to select on a file descriptor and a
// timer together without one.
func (a *app) run(in *os.File) error {
	keys := make(chan key, 8)
	errs := make(chan error, 1)
	go func() {
		kr := newKeyReader(in)
		for {
			k, err := kr.next()
			if err != nil {
				errs <- err
				return
			}
			keys <- k
		}
	}()

	tick := time.NewTicker(a.interval)
	defer tick.Stop()

	a.draw(true)

	for {
		select {
		case k := <-keys:
			force := k.t == keyCtrlL
			if !a.m.handleKey(k) {
				return nil
			}
			a.draw(force)
		case <-tick.C:
			// The periodic re-read is live state only: the daemon's peers,
			// bans, routes and interfaces. The config file is not re-read on
			// a timer — it changes when somebody changes it, and re-reading
			// a multi-megabyte config three times a second to notice would
			// be a poor trade. The r key does that.
			//
			// This replaces a.m.snap with a new value rather than mutating
			// the existing one in place — see snapshot.refreshedLive's own
			// comment for why: a lazy fetch started from the last page build
			// may still be reading the old snapshot from its own goroutine,
			// and mutating shared fields out from under it is a data race.
			if a.m.snap != nil {
				a.m.snap = a.m.snap.refreshedLive()
			}
			// Metrics is the one lazy value that is a *reading* rather than a
			// document, so it is the one that goes stale by sitting still.
			a.m.lazy.invalidate("metrics")
			a.draw(false)
		case <-a.m.lazy.wake:
			a.draw(false)
		case err := <-errs:
			return err
		}
	}
}

// draw renders a frame and writes the difference. force discards the previous
// frame, which is what Ctrl-L and a resize both need.
func (a *app) draw(force bool) {
	w, h, ok := termSize(a.out)
	if !ok {
		w, h = 80, 24
	}
	if a.prev != nil {
		if pw, ph := a.prev.Size(); pw != w || ph != h {
			force = true
		}
	}
	if force {
		a.prev = nil
	}
	s := NewScreen(w, h)
	a.m.draw(s)
	if out := s.render(a.prev, a.m.cm); out != "" {
		_, _ = a.out.WriteString(out)
	}
	a.prev = s
}
