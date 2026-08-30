package tui

// The content model: what a page is made of, and how it becomes lines.
//
// Every page in sections.go builds a []card and returns it. Nothing in there
// knows the terminal's width, computes a column, or picks a colour — a page
// is a description of what to show, and layout() below is the single place
// that turns descriptions into pixels. That split is what makes a page's test
// a few lines long (build it, layout it at a fixed width, assert on the text)
// and what keeps forty-two pages from having forty-two subtly different ideas
// of how a table is spaced.
//
// A laid-out page is []line, and a line is []span — a run of text with one
// style. Scrolling is then slicing that slice, and the search highlight is
// restyling one span, both of which are considerably easier to get right than
// re-rendering a page at an offset.

import (
	"strings"
	"unicode/utf8"
)

// span is a run of text in one style.
type span struct {
	text string
	st   style
}

// line is one rendered row.
type line []span

// text returns a line's plain text, which is what most tests assert on.
func (l line) text() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.text)
	}
	return b.String()
}

// width reports how many cells a line occupies.
func (l line) width() int {
	n := 0
	for _, s := range l {
		n += utf8.RuneCountInString(s.text)
	}
	return n
}

// ---- the model ----------------------------------------------------------

// card is one bordered panel, matching the web admin's .card: a heading and
// a body of items.
type card struct {
	title string
	items []item
}

// item is one thing inside a card. The set is deliberately small — five
// shapes cover all forty-two pages, and a sixth would mean a page wanted
// something bespoke, which is the moment to ask whether the page is right
// rather than to grow this.
type item interface{ isItem() }

// kv is a block of label/value rows, the shape of the web admin's
// .settings-row and .info-kv. Labels are right-padded to a common width so
// values line up down the card.
type kv struct {
	rows []kvRow
}

// kvRow is one label/value pair. tone selects the value's colour: "" for
// ordinary, "ok", "warn", "danger", or "mut" for a value that is present but
// not interesting (a default, an empty list).
type kvRow struct {
	k, v string
	tone string
}

// table is a header row plus body rows. Column widths are computed from the
// content by layout(), which also decides what to drop when the terminal is
// too narrow — see layoutTable.
type table struct {
	head []string
	rows []tableRow

	// selectKey and ids mark this table as containing selectable rows, for
	// pages with editing actions (see rows.go and app.go's row-selection
	// handling). selectKey names which entity these rows are — a page can
	// have more than one selectable table (Firewall has one rule table per
	// network plus the exemptions table), and selectKey is what tells the
	// action layer which one a given selection belongs to. ids holds one
	// stable identifier per row, parallel to rows — a network name, a
	// firewall rule id, a node id — used instead of a row index so the
	// cursor survives a refresh even if the list's order shifts underneath
	// it. A read-only table leaves both empty, which is what keeps this
	// opt-in: every existing table literal in this tree, and everything
	// content_test.go already asserts, is unaffected.
	selectKey string
	ids       []string
}

// tableRow is one row, with an optional tone applied to the whole row (a
// disabled rule is drawn dim, a banned peer in danger) and an optional
// per-cell tone override.
type tableRow struct {
	cells []string
	tone  string
	// cellTone, when non-nil, is a per-column tone that wins over tone.
	// Sparse by design: a nil entry means "use the row's tone".
	cellTone map[int]string
}

// para is prose, wrapped to the card's width.
type para struct {
	text string
	tone string
}

// mono is pre-formatted text: log lines, a routing table, a document. Not
// wrapped — these are things whose columns mean something, and a wrapped
// route table is unreadable — so long lines are truncated at the edge and the
// page scrolls horizontally instead. See app.go's hscroll.
type mono struct {
	lines []string
	tone  string
}

// empty is the "nothing here" state, matching the web admin's .empty. A
// distinct item rather than a para so it is styled consistently and so a page
// cannot accidentally render an empty list as blank space, which reads as a
// failed load rather than as an empty list.
type empty struct{ msg string }

func (kv) isItem()    {}
func (table) isItem() {}
func (para) isItem()  {}
func (mono) isItem()  {}
func (empty) isItem() {}

// ---- layout -------------------------------------------------------------

// layoutCtx carries what layout needs and a page does not: the palette and
// the width available inside the content pane.
type layoutCtx struct {
	pal   palette
	width int

	// selTableKey and selRowID identify the one row, across the whole page,
	// that should be drawn as selected — compared against each selectable
	// table's own selectKey/ids by layoutTable. Both empty (the zero value)
	// means nothing is selected, which is the case for every page with no
	// selectable tables and every existing test: this is pure addition,
	// nothing downstream of an unset ctx changes behavior.
	selTableKey string
	selRowID    string
}

// toneStyle maps a tone name to a style. One place, so "danger" means the
// same red on every page.
func (c layoutCtx) toneStyle(tone string) style {
	switch tone {
	case "ok":
		return style{}.withFg(c.pal.ok)
	case "warn":
		return style{}.withFg(c.pal.warn)
	case "danger":
		return style{}.withFg(c.pal.danger)
	case "mut":
		return style{}.withFg(c.pal.mut)
	case "acc":
		return style{}.withFg(c.pal.acc)
	case "dim":
		return style{}.withFg(c.pal.mut).withDim()
	}
	return style{}.withFg(c.pal.fg)
}

// Box-drawing characters for card borders. Light rather than heavy or
// double: the web admin's card border is a single 1px --line rule, and the
// terminal equivalent of a thin neutral rule is the light set.
const (
	boxTL, boxTR, boxBL, boxBR = '\u250c', '\u2510', '\u2514', '\u2518'
	boxH, boxV                 = '\u2500', '\u2502'
)

// layout renders a page's cards to lines at a given width.
func layout(cards []card, c layoutCtx) []line {
	var out []line
	for i, cd := range cards {
		if i > 0 {
			out = append(out, line{})
		}
		out = append(out, layoutCard(cd, c)...)
	}
	return out
}

// layoutCard draws one card: a top border carrying the title, the body inset
// by one cell on each side, and a bottom border.
func layoutCard(cd card, c layoutCtx) []line {
	w := c.width
	if w < 8 {
		w = 8
	}
	lineSt := style{}.withFg(c.pal.line)
	titleSt := style{}.withFg(c.pal.mut).withBold()

	// Top border: ┌─ TITLE ─────┐, with the title upper-cased the way
	// .card h3's text-transform does.
	title := strings.ToUpper(cd.title)
	top := line{{string(boxTL) + string(boxH), lineSt}}
	used := 2
	if title != "" {
		t := truncate(title, max(0, w-6))
		top = append(top, span{" " + t + " ", titleSt})
		used += utf8.RuneCountInString(t) + 2
	}
	if fill := w - used - 1; fill > 0 {
		top = append(top, span{strings.Repeat(string(boxH), fill), lineSt})
	}
	top = append(top, span{string(boxTR), lineSt})

	out := []line{top}

	inner := layoutCtx{pal: c.pal, width: w - 4, selTableKey: c.selTableKey, selRowID: c.selRowID} // one border + one pad each side
	var body []line
	for i, it := range cd.items {
		if i > 0 {
			body = append(body, line{})
		}
		body = append(body, layoutItem(it, inner)...)
	}
	for _, bl := range body {
		row := line{{string(boxV) + " ", lineSt}}
		row = append(row, bl...)
		if pad := w - 3 - bl.width(); pad > 0 {
			row = append(row, span{strings.Repeat(" ", pad), style{}})
		}
		row = append(row, span{string(boxV), lineSt})
		out = append(out, row)
	}

	bottom := line{{string(boxBL) + strings.Repeat(string(boxH), max(0, w-2)) + string(boxBR), lineSt}}
	return append(out, bottom)
}

// layoutItem dispatches one item.
func layoutItem(it item, c layoutCtx) []line {
	switch v := it.(type) {
	case kv:
		return layoutKV(v, c)
	case editableKV:
		return layoutEditableKV(v, c)
	case table:
		return layoutTable(v, c)
	case para:
		return layoutPara(v, c)
	case mono:
		return layoutMono(v, c)
	case empty:
		return []line{{{v.msg, style{}.withFg(c.pal.mut)}}}
	}
	return nil
}

// layoutKV renders label/value rows with the labels padded to a common
// width. The label column is capped at a third of the available space so a
// single long label cannot push every value off the right edge.
// layoutKV renders label/value rows with the labels padded to a common
// width. The label column is capped at a third of the available space so a
// single long label cannot push every value off the right edge.
func layoutKV(v kv, c layoutCtx) []line {
	kw := 0
	for _, r := range v.rows {
		if n := utf8.RuneCountInString(r.k); n > kw {
			kw = n
		}
	}
	if cap := c.width / 3; kw > cap && cap > 0 {
		kw = cap
	}
	keySt := style{}.withFg(c.pal.mut)
	var out []line
	for _, r := range v.rows {
		k := truncate(r.k, kw)
		pad := kw - utf8.RuneCountInString(k) + 2
		val := truncate(r.v, max(0, c.width-kw-2))
		out = append(out, line{
			{k + strings.Repeat(" ", pad), keySt},
			{val, c.toneStyle(r.tone)},
		})
	}
	return out
}

// editableKVRow is one directly-editable field: rendered exactly like a
// kvRow (an aligned label/value pair) but carrying the mnemonic machinery
// kvRow deliberately doesn't — see editableKV's own comment for why these
// are a separate type rather than new fields bolted onto kvRow.
type editableKVRow struct {
	k, v, tone string

	// edit marks this row as directly editable: a page-unique mnemonic
	// character is assigned to it (mnemonics.go) and pressing that key, with
	// no row navigation first, opens the form edit returns. nil means this
	// particular row — inside an otherwise-editable block — is informational
	// only (a read-only status sitting alongside settings it explains) and
	// gets no mnemonic.
	edit func(m *model) formSpec

	// mnemonic is written by assignMnemonicsInPlace right before a page is
	// laid out or a key is dispatched; page builders never set it.
	mnemonic rune
}

// editableKV is a block of directly-editable label/value rows — the same
// aligned-column look kv gives a block of purely informational ones, plus a
// mnemonic underline on each row's label (see mnemonics.go) that jumps
// straight to editing it.
//
// This is a second type rather than new fields on kv/kvRow on purpose: kv
// is used in roughly three dozen places across this package already, every
// one of them a plain positional literal (`kvRow{"cpu", pct, tone}`), and
// Go's positional composite literals require every field to be given —
// adding fields to kvRow would have required touching every one of those
// call sites just to append zero values they don't need. Keeping the two
// types separate means every existing read-only page is completely
// unaffected, and a page gains mnemonics only where a builder deliberately
// reaches for editableKV instead of kv — visible at the call site, not an
// invisible property every kv block now has to think about.
type editableKV struct {
	rows []editableKVRow
}

func (editableKV) isItem() {}

// layoutEditableKV is layoutKV's column-alignment logic, plus the mnemonic
// underline. Display labels (a "[x] " prefix for a mnemonic that isn't one
// of the label's own letters — see pickMnemonic) are computed before the
// column width, not after, so that prefix is measured the same as any other
// character rather than silently overflowing the column it's padded to.
func layoutEditableKV(v editableKV, c layoutCtx) []line {
	type displayRow struct {
		label       string
		mnemonicPos int // rune index of the mnemonic within label, -1 if none
	}
	disp := make([]displayRow, len(v.rows))
	kw := 0
	for i, r := range v.rows {
		label, pos := r.k, -1
		if r.edit != nil && r.mnemonic != 0 {
			if p := runeIndexFold(r.k, r.mnemonic); p >= 0 {
				label, pos = r.k, p
			} else {
				label = "[" + string(r.mnemonic) + "] " + r.k
				pos = 1
			}
		}
		disp[i] = displayRow{label: label, mnemonicPos: pos}
		if n := utf8.RuneCountInString(label); n > kw {
			kw = n
		}
	}
	if cap := c.width / 3; kw > cap && cap > 0 {
		kw = cap
	}
	keySt := style{}.withFg(c.pal.mut)
	mnemonicSt := style{}.withFg(c.pal.acc).withBold().withUnderline()

	var out []line
	for i, r := range v.rows {
		label := truncate(disp[i].label, kw)
		pad := kw - utf8.RuneCountInString(label) + 2
		val := truncate(r.v, max(0, c.width-kw-2))

		var keySpans []span
		if pos := disp[i].mnemonicPos; pos >= 0 && pos < utf8.RuneCountInString(label) {
			rs := []rune(label)
			if before := string(rs[:pos]); before != "" {
				keySpans = append(keySpans, span{before, keySt})
			}
			keySpans = append(keySpans, span{string(rs[pos]), mnemonicSt})
			if after := string(rs[pos+1:]); after != "" {
				keySpans = append(keySpans, span{after, keySt})
			}
		} else {
			keySpans = append(keySpans, span{label, keySt})
		}
		keySpans = append(keySpans, span{strings.Repeat(" ", pad), keySt})

		row := append(line{}, keySpans...)
		row = append(row, span{val, c.toneStyle(r.tone)})
		out = append(out, row)
	}
	return out
}

// runeIndexFold returns the rune index of the first case-insensitive match
// of target within s, or -1. Used to find where a mnemonic sits in its own
// label — mnemonics are assigned in lowercase, but the label likely isn't.
func runeIndexFold(s string, target rune) int {
	lower := unicodeToLower(target)
	i := 0
	for _, r := range s {
		if unicodeToLower(r) == lower {
			return i
		}
		i++
	}
	return -1
}

func unicodeToLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// layoutTable renders a header, a rule, and the body rows.
//
// Column sizing is the interesting part. Every column gets the width of its
// widest cell, and if that overflows the available space the surplus is taken
// from the widest columns first, one cell at a time, down to a floor of six
// characters. Taking from the widest is what keeps a table of one long notes
// column and six short ones from truncating the short ones into uselessness —
// the notes column absorbs nearly all of the loss, which is also the column
// where losing the tail costs least.
//
// A selectable table (selectKey != "") reserves a fixed two-cell gutter on
// the left for a cursor marker, present whether or not any row is currently
// selected — reserved rather than conditional, so the table's columns don't
// visibly shift width the moment a selection first appears on it. Structural
// invariant relied on elsewhere (row_test.go pins it): this always returns
// exactly 2 + len(rows) lines — header, rule, one line per row, in order —
// which is what lets the auto-scroll logic in app.go locate a selected row's
// line without re-implementing this function, only re-calling it.
func layoutTable(t table, c layoutCtx) []line {
	n := len(t.head)
	if n == 0 {
		return nil
	}
	const gap = 2
	const floor = 6
	gutter := 0
	if t.selectKey != "" {
		gutter = 2
	}
	avail := c.width - gutter

	widths := make([]int, n)
	for i, h := range t.head {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range t.rows {
		for i := 0; i < n && i < len(r.cells); i++ {
			if w := utf8.RuneCountInString(r.cells[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	total := gap * (n - 1)
	for _, w := range widths {
		total += w
	}
	for total > avail {
		widest, wi := 0, -1
		for i, w := range widths {
			if w > widest && w > floor {
				widest, wi = w, i
			}
		}
		if wi < 0 {
			break // every column is at the floor; the table will be clipped
		}
		widths[wi]--
		total--
	}

	headSt := style{}.withFg(c.pal.mut).withBold()
	var out []line

	head := make(line, 0, n*2+1)
	if gutter > 0 {
		head = append(head, span{"  ", style{}})
	}
	for i, h := range t.head {
		if i > 0 {
			head = append(head, span{strings.Repeat(" ", gap), style{}})
		}
		head = append(head, span{padTo(h, widths[i]), headSt})
	}
	out = append(out, head)

	rule := strings.Repeat(" ", gutter) + strings.Repeat(string(boxH), max(0, min(total, avail)))
	out = append(out, line{{rule, style{}.withFg(c.pal.line)}})

	for i, r := range t.rows {
		selected := t.selectKey != "" && t.selectKey == c.selTableKey && i < len(t.ids) && t.ids[i] == c.selRowID

		row := make(line, 0, n*2+1)
		if gutter > 0 {
			marker, markerSt := "  ", style{}
			if selected {
				marker, markerSt = "\u25b8 ", style{}.withFg(c.pal.acc).withBold()
			}
			row = append(row, span{marker, markerSt})
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				row = append(row, span{strings.Repeat(" ", gap), style{}})
			}
			cellText := ""
			if i < len(r.cells) {
				cellText = r.cells[i]
			}
			var cellSt style
			if selected {
				cellSt = style{}.withFg(c.pal.fg).withBg(c.pal.hover)
			} else {
				tone := r.tone
				if r.cellTone != nil {
					if t, ok := r.cellTone[i]; ok {
						tone = t
					}
				}
				cellSt = c.toneStyle(tone)
			}
			row = append(row, span{padTo(cellText, widths[i]), cellSt})
		}
		out = append(out, row)
	}
	return out
}

// layoutPara wraps prose at the card's width.
func layoutPara(p para, c layoutCtx) []line {
	st := c.toneStyle(p.tone)
	if p.tone == "" {
		st = style{}.withFg(c.pal.mut)
	}
	var out []line
	for _, w := range wrap(p.text, c.width) {
		out = append(out, line{{w, st}})
	}
	return out
}

// layoutMono renders pre-formatted lines, truncated rather than wrapped.
func layoutMono(m mono, c layoutCtx) []line {
	st := c.toneStyle(m.tone)
	if m.tone == "" {
		st = style{}.withFg(c.pal.fg)
	}
	out := make([]line, 0, len(m.lines))
	for _, l := range m.lines {
		out = append(out, line{{truncate(expandTabs(l), c.width), st}})
	}
	if len(out) == 0 {
		out = append(out, line{{"(empty)", style{}.withFg(c.pal.mut)}})
	}
	return out
}

// ---- text helpers -------------------------------------------------------

// wrap breaks text into lines of at most w cells, at spaces where it can and
// mid-word where it must. Explicit newlines in the input are honored as
// paragraph breaks.
func wrap(text string, w int) []string {
	if w < 1 {
		w = 1
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		cur := ""
		for _, word := range words {
			for utf8.RuneCountInString(word) > w {
				// A single word longer than the line: break it rather than
				// overflow. Rare, but a base64 key or a long path hits it.
				if cur != "" {
					out = append(out, cur)
					cur = ""
				}
				out = append(out, string([]rune(word)[:w]))
				word = string([]rune(word)[w:])
			}
			switch {
			case cur == "":
				cur = word
			case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= w:
				cur += " " + word
			default:
				out = append(out, cur)
				cur = word
			}
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	return out
}

// padTo right-pads (or truncates) s to exactly w cells.
func padTo(s string, w int) string {
	s = truncate(s, w)
	if n := w - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// expandTabs turns tabs into spaces on an eight-column grid, since the screen
// has no notion of a tab stop and a raw tab would be drawn as one space,
// misaligning exactly the pre-formatted output mono exists to preserve.
func expandTabs(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
