package tui

// The bridge between a page's data (a []card, rebuilt fresh every time it's
// needed — see sections.go) and the model's idea of "which row is the
// cursor on right now." Two things live here: enumerating every selectable
// row on a page in document order, and finding the exact on-screen line one
// of them lands on so the viewport can be scrolled to keep it visible.
//
// Both are written as thin wrappers around the real layout functions rather
// than a second description of a card's structure, for the reason given on
// table's own selectKey field and pinned by TestLayoutTableStructuralInvariant
// below: a hand-maintained line-counting formula that quietly drifted from
// what layoutCard/layoutTable actually draw would be a much worse bug than
// the one this file exists to avoid — the cursor would highlight one row
// while the viewport scrolled to a different one, and nothing would fail
// loudly enough to notice.

// selRow is one selectable row, as found by walking a page's cards in the
// same order layout() draws them.
type selRow struct {
	tableKey string // the table's own selectKey
	id       string // that row's stable id, from the table's ids slice

	// Location within the card tree, kept only so rowLineOffset can find
	// this row again without re-walking from the top — not meant to be
	// compared or relied on by anything outside this file.
	cardIdx int
	itemIdx int
	rowIdx  int
}

// flattenSelectable walks cards in exactly the order layout() renders them —
// card by card, item by item within a card, row by row within a table — and
// lists every row belonging to a selectable table. This order is what makes
// up/down a single, predictable cursor across a page that may hold more than
// one selectable table (Firewall's per-network rule tables plus its
// exemptions table, for instance): the cursor moves through all of them in
// the same top-to-bottom order a reader's eye would.
func flattenSelectable(cards []card) []selRow {
	var out []selRow
	for ci, cd := range cards {
		for ii, it := range cd.items {
			t, ok := it.(table)
			if !ok || t.selectKey == "" {
				continue
			}
			for ri := range t.rows {
				id := ""
				if ri < len(t.ids) {
					id = t.ids[ri]
				}
				out = append(out, selRow{tableKey: t.selectKey, id: id, cardIdx: ci, itemIdx: ii, rowIdx: ri})
			}
		}
	}
	return out
}

// findSelRow returns the entry in rows matching (tableKey, id), and whether
// one was found. Used to re-locate the current selection after a refresh may
// have changed the list, and to find where in rows the current selection
// sits so up/down can step to its neighbor.
func findSelRow(rows []selRow, tableKey, id string) (int, bool) {
	if tableKey == "" && id == "" {
		return -1, false
	}
	for i, r := range rows {
		if r.tableKey == tableKey && r.id == id {
			return i, true
		}
	}
	return -1, false
}

// rowLineOffset returns the on-screen line (relative to the start of the
// laid-out cards, before headLen is added by the caller) where row's line
// begins, by re-invoking layoutCard/layoutItem on the prefix of cards/items
// that precede it and taking the length of what they actually produce.
//
// This costs a handful of extra layout calls on the one page, the one frame,
// where a selection changed — not a hot loop, and layout itself is
// microseconds at these sizes (see app.go's contentLines). What it buys is
// that this can never disagree with what actually gets drawn: it isn't
// counting lines by a separate formula, it's asking the real formatter how
// many lines its own prefix produced.
func rowLineOffset(cards []card, ctx layoutCtx, row selRow) int {
	// Every full card before row's own card, plus one blank separator line
	// before each of them except the very first — the same rule layout()
	// itself applies (see its own "if i > 0" line).
	offset := 0
	for i := 0; i < row.cardIdx; i++ {
		if i > 0 {
			offset++
		}
		offset += len(layoutCard(cards[i], ctx))
	}
	if row.cardIdx > 0 {
		offset++
	}

	// Within row's own card: the top border line, plus every item before
	// row's item and the blank line that precedes each of them but the
	// first — mirroring layoutCard's own body loop exactly, because it is
	// that loop, called on a prefix. Items are laid out at the same inner
	// width layoutCard itself would give them (see cardInnerWidth), so a
	// wrapped para or mono block before the target table reflows identically
	// and its line count is accurate, not just its presence.
	offset++ // top border
	cd := cards[row.cardIdx]
	inner := layoutCtx{pal: ctx.pal, width: cardInnerWidth(ctx.width)}
	for i := 0; i < row.itemIdx; i++ {
		if i > 0 {
			offset++
		}
		offset += len(layoutItem(cd.items[i], inner))
	}
	if row.itemIdx > 0 {
		offset++
	}

	// Within row's own table: header + rule, then one line per row before
	// this one — the structural invariant TestLayoutTableStructuralInvariant
	// pins, rather than a magic number re-derived here.
	offset += 2 + row.rowIdx
	return offset
}

// cardInnerWidth mirrors the arithmetic layoutCard uses to size its own
// inner context — the same "clamp to at least 8, then subtract one border
// and one pad on each side" layoutCard itself applies — so rowLineOffset's
// re-invocation of layoutItem on a card's items sees the same width
// layoutCard would actually have given them, and therefore reflows any
// wrapped prose identically.
func cardInnerWidth(outer int) int {
	w := outer
	if w < 8 {
		w = 8
	}
	return w - 4
}
