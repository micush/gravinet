package tui

// The top bar's search, and the help page the ? key opens.
//
// The web admin's global search indexes page names and the controls on them —
// it can take you to a specific row on Settings. This indexes pages: their
// key, their rail label, and their description. Indexing further would mean a
// second description of what is on each page, maintained alongside the page
// itself, and the thing that makes that worth having in a browser is that
// clicking a result can scroll to and flash a specific control. There is
// nothing to flash here, so the honest scope is "take me to the page", which
// is what most of the browser's own use of it is anyway.

import "strings"

// helpSection is the key bindings page. Not in navGroups, because it has no
// counterpart in the rail — it is the terminal's substitute for a mouse.
const helpSection = "help"

// searchHit is one match, with a score that orders the list.
type searchHit struct {
	sec   string
	score int
}

// searchSections ranks pages against a query. An empty query lists every
// visible page in rail order, which makes the search box double as a flat
// index of the whole console — useful on a narrow terminal where the rail is
// mostly collapsed.
//
// Scoring, best first: an exact key match, then a label or key prefix, then a
// substring of either, then a description substring. Ties keep rail order,
// which is why this walks navGroups rather than a map.
func searchSections(query string, c caps) []searchHit {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []searchHit
	for _, sec := range sectionKeys() {
		if !sectionVisible(sec, c) {
			continue
		}
		if q == "" {
			out = append(out, searchHit{sec: sec, score: 0})
			continue
		}
		key := strings.ToLower(sec)
		lbl := strings.ToLower(label(sec))
		desc := strings.ToLower(descFor(sec))
		grp := strings.ToLower(groupFor(sec))
		switch {
		case key == q || lbl == q:
			out = append(out, searchHit{sec, 0})
		case strings.HasPrefix(key, q) || strings.HasPrefix(lbl, q):
			out = append(out, searchHit{sec, 1})
		case strings.Contains(key, q) || strings.Contains(lbl, q):
			out = append(out, searchHit{sec, 2})
		case grp != "" && strings.HasPrefix(grp, q):
			out = append(out, searchHit{sec, 3})
		case strings.Contains(desc, q):
			out = append(out, searchHit{sec, 4})
		}
	}
	// A stable sort by score preserves rail order within a score band, which
	// is what makes the empty-query listing read as the rail flattened.
	stableSortByScore(out)
	return out
}

// stableSortByScore is an insertion sort. The list is at most forty-three
// entries and this runs on a keystroke; sort.SliceStable would be one more
// import for something that is measurably free either way, and insertion sort
// is stable by construction.
func stableSortByScore(hits []searchHit) {
	for i := 1; i < len(hits); i++ {
		h := hits[i]
		j := i - 1
		for j >= 0 && hits[j].score > h.score {
			hits[j+1] = hits[j]
			j--
		}
		hits[j+1] = h
	}
}

// handleSearchKey drives the overlay. Enter opens the highlighted hit, Escape
// closes without moving, and typing re-ranks on every keystroke — the same
// behaviour as the browser's filter box.
func (m *model) handleSearchKey(k key) bool {
	switch k.t {
	case keyCtrlC, keyCtrlD:
		return false
	case keyEsc:
		m.searching = false
	case keyEnter:
		if m.matchIdx < len(m.matches) {
			m.setSection(m.matches[m.matchIdx].sec)
		}
		m.searching = false
	case keyUp:
		if m.matchIdx > 0 {
			m.matchIdx--
		}
	case keyDown:
		if m.matchIdx < len(m.matches)-1 {
			m.matchIdx++
		}
	case keyBackspace:
		if r := []rune(m.query); len(r) > 0 {
			m.query = string(r[:len(r)-1])
			m.rerank()
		}
	case keyCtrlU:
		m.query = ""
		m.rerank()
	case keyRune:
		m.query += string(k.r)
		m.rerank()
	}
	return true
}

func (m *model) rerank() {
	m.matches = searchSections(m.query, m.caps())
	m.matchIdx = 0
}

// nextMatch cycles through the last search's results without reopening the
// box — the n / N pair, which is what makes a search for "dns" a two-key way
// to flip between Naming › DNS and Monitor › DNS State.
func (m *model) nextMatch(delta int) {
	if len(m.matches) == 0 {
		m.flash = "no active search (press / to search)"
		return
	}
	m.matchIdx = (m.matchIdx + delta + len(m.matches)) % len(m.matches)
	m.setSection(m.matches[m.matchIdx].sec)
}

// helpKeys is the binding table, and the only description of the bindings
// that exists: drawFooter's summary line and the help page are both rendered
// from it, so a binding cannot be added without appearing in both.
var helpKeys = [][2]string{
	{"tab", "move focus between the rail and the page"},
	{"\u2191 \u2193 / k j", "move the rail cursor, or scroll the page"},
	{"pgup pgdn / space", "scroll a screenful"},
	{"g / G", "jump to the top or bottom of the page"},
	{"\u2190 \u2192", "focus the rail, or scroll a wide page sideways"},
	{"enter", "open the page, or expand the group, under the cursor"},
	{"/", "search every page by name or description"},
	{"n / N", "jump to the next or previous search hit"},
	{"r", "re-read the config, the daemon, and every cached page"},
	{"t", "switch between the dark and light palettes"},
	{"ctrl-l", "repaint, if something else has written over the screen"},
	{"?", "this page"},
	{"q / ctrl-c", "quit"},
}

// pageHelp renders the bindings. Registered in pageBuilders like any other
// page, so it scrolls and searches the same way the rest do.
func pageHelp(c pageCtx) []card {
	t := table{head: []string{"key", "does"}}
	for _, b := range helpKeys {
		t.rows = append(t.rows, tableRow{cells: []string{b[0], b[1]}})
	}
	return []card{
		{title: "keys", items: []item{t}},
		{title: "about this console", items: []item{para{
			text: "This is the web admin's own layout — the same groups, the same pages, in the same order — reading " +
				"the same config file and the same control socket the CLI reads. It does not edit: every page that " +
				"has an editor names the command that reaches it, at the bottom of the page.", tone: "mut"}}},
	}
}

func init() {
	// Registered here rather than in the literal so the help page sits next
	// to the binding table it renders.
	pageBuilders[helpSection] = pageHelp
}
