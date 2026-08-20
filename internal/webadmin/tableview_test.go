package webadmin

import (
	"strings"
	"testing"
)

// enhanceTableSrc returns the body of the embedded UI's enhanceTable, bounded
// by the next top-level function so an assertion below cannot be satisfied by
// something written elsewhere on the page.
func enhanceTableSrc(t *testing.T) string {
	t.Helper()
	i := strings.Index(indexHTML, "function enhanceTable(table)")
	if i < 0 {
		t.Fatal("enhanceTable not found in indexHTML")
	}
	body := indexHTML[i:]
	j := strings.Index(body, "\nfunction addLineFilter")
	if j < 0 {
		t.Fatal("could not find the end of enhanceTable; the bound below would be unreliable")
	}
	return body[:j]
}

// TestTableViewSurvivesRerender guards against reintroducing "sort or filter a
// table in the admin UI, and the next automatic refresh undoes it".
//
// enhanceTable only ever reorders and hides existing <tr> nodes — it does not
// touch the data behind them — and every table it enhances is thrown away and
// rebuilt by renderSection(). The chosen column and direction lived in
// `col`/`dir`, and the filter text lived in the <input> itself; all three
// belonged to the destroyed DOM, so they died with it. On Mesh > peers,
// Monitor > Mesh Peers and Bans, startPolling() re-renders on any status
// change every four seconds, which meant a sort could be undone faster than
// the table could be read. The filter hid it slightly better: the poll skips
// re-rendering while an input has focus, so the box only emptied once the
// operator clicked away from it.
//
// The fix keeps both in state.tableView, keyed by a description of the table
// (section, card, columns) rather than by an object that does not survive, and
// re-applies them as the last step of enhancing a table.
//
// This scans the served page rather than running the JS: this package has no
// JS runtime dependency in its test suite (TestUIScriptParses is opt-in and
// skips without node). So this pins the wiring, not the behaviour — it cannot
// prove a view is restored, only that nothing has removed the parts that do
// it.
func TestTableViewSurvivesRerender(t *testing.T) {
	if !strings.Contains(indexHTML, "tableView:{}") {
		t.Fatal("state.tableView is gone — a table's sort and filter have nowhere to live across a re-render, since the table itself does not survive one")
	}
	if !strings.Contains(indexHTML, "function tableViewKey(table)") {
		t.Fatal("tableViewKey is missing — without a stable identity a table cannot find its own saved view after being rebuilt")
	}

	body := enhanceTableSrc(t)

	if !strings.Contains(body, "const key = tableViewKey(table);") {
		t.Error("enhanceTable no longer derives a view key, so nothing can be saved or restored under it")
	}
	if !strings.Contains(body, "const view = state.tableView[key] || (state.tableView[key] = {});") {
		t.Error("enhanceTable no longer looks up (or creates) this table's view record")
	}

	// Sort: recorded on click, re-applied on enhance.
	if !strings.Contains(body, "view.col = i; view.dir = d;") {
		t.Error("the header click no longer records the chosen column and direction; the sort will again be lost with the table")
	}
	if !strings.Contains(body, "if (sortable) sortBy(view.col, view.dir);") {
		t.Error("enhanceTable no longer re-applies a saved sort — saving it without restoring it is the same bug with extra steps")
	}

	// Filter: recorded on input, put back before the sort restore.
	if !strings.Contains(body, "filt.oninput = () => { view.filter = filt.value; applyFilter(); };") {
		t.Error("the filter box no longer records what was typed into it; its text will again be lost the moment focus leaves and a poll re-renders")
	}
	if !strings.Contains(body, "if (filt && view.filter) filt.value = view.filter;") {
		t.Error("enhanceTable no longer restores the filter text")
	}
	// A restored filter has to be applied even when there is no sort to
	// restore, or the box comes back populated over an unfiltered table —
	// which is worse than losing it, since the visible rows then contradict
	// the query sitting above them.
	if !strings.Contains(body, "else applyFilter();") {
		t.Error("a table with a restored filter and no saved sort never applies the filter — the box would show a query the rows do not obey")
	}
	if strings.Index(body, "if (filt && view.filter) filt.value = view.filter;") > strings.Index(body, "const sortBy = (i, d) => {") {
		t.Error("the filter is restored after the sort, so the sort's own applyFilter() runs against an empty box and the rows are filtered in a second pass")
	}

	// The restore has to reuse the reordering, not the click handler: a
	// restore that went through the toggle would flip a remembered 'desc' back
	// to 'asc' on every poll, which looks like the table sorting itself.
	i := strings.Index(body, "const sortBy = (i, d) => {")
	if i < 0 {
		t.Fatal("sortBy is gone; the reordering and the direction toggle have been folded back together")
	}
	j := strings.Index(body[i:], "\n  };")
	if j < 0 {
		t.Fatal("could not bound sortBy")
	}
	if sortByBody := body[i : i+j]; strings.Contains(sortByBody, "-dir") {
		t.Error("sortBy toggles the direction itself — a restore calling it would invert the saved order on every re-render")
	}

	// The key must describe the table, not its position in the render: an
	// index moves as soon as a network is added or a card appears, and would
	// hand one table's view to another.
	k := strings.Index(indexHTML, "function tableViewKey(table)")
	keyBody := indexHTML[k:]
	if e := strings.Index(keyBody, "\nfunction "); e > 0 {
		keyBody = keyBody[:e]
	}
	if !strings.Contains(keyBody, "header.cells") {
		t.Error("the view key no longer includes the column signature — a renamed or reordered column would replay a stale sort against an index that now means something else")
	}
	if !strings.Contains(keyBody, "state.section") {
		t.Error("the view key no longer includes the section, so two sections' identically-shaped tables would share one saved view")
	}
}

// TestTableViewClearedOnTargetSwitch: a remembered sort is harmless on another
// managed node, but a remembered filter is not. Carrying "rpi4" onto a node
// with no such peer shows an empty table under a filter box the operator did
// not type there, which reads as the node having no peers at all. setTarget is
// the one place a real switch is known to have happened (see its own comment,
// and _ifaceCache beside it for the same reasoning applied to a stale
// interface list), so the reset belongs there and nowhere else.
func TestTableViewClearedOnTargetSwitch(t *testing.T) {
	i := strings.Index(indexHTML, "function setTarget(v)")
	if i < 0 {
		t.Fatal("setTarget not found in indexHTML")
	}
	line := indexHTML[i:]
	if e := strings.Index(line, "\n"); e > 0 {
		line = line[:e]
	}
	if !strings.Contains(line, "state.tableView = {}") {
		t.Errorf("setTarget does not clear state.tableView — one node's filter would follow the operator to the next, hiding rows that are actually there.\ngot: %s", line)
	}
}

// TestPeersStateColumnIsContentSized: Mesh > peers' first two columns held 38%
// and 20% of the table between them, and neither one's content scales with the
// table. State is a closed set of short labels — enabled, disabled,
// connecting…, this node. Target is nodeCell's "hostname id", where the id is
// a fixed 16 hex characters and only the hostname varies. On any ordinary
// window most of both columns was empty, which is what put a visible gap
// after the hostname and again after the state tag.
//
// It is sized in px now, which is the actual point: a percentage tracks the
// table's width, and this column's content does not. The set of values is
// closed, so the right width is the widest of them plus padding, once, at any
// window size. The width it releases goes to the c-fill columns (endpoint,
// notes), which are auto and do hold variable-length values.
//
// This pins the shape rather than the number — the number is a measurement
// (86px for "connecting…" at 13px monospace, plus 20px of th,td padding) and
// may be re-measured, but going back to a percentage would be the regression.
func TestPeersStateColumnIsContentSized(t *testing.T) {
	for _, col := range []string{"c-state-op", "c-target-op"} {
		i := strings.Index(indexHTML, "table.peers-table col."+col+" {")
		if i < 0 {
			t.Fatalf("the %s rule is gone; that column has no explicit width under table-layout:fixed", col)
		}
		rule := indexHTML[i:]
		if e := strings.Index(rule, "}"); e > 0 {
			rule = rule[:e]
		}
		if strings.Contains(rule, "%") {
			t.Errorf("%s is a percentage again (%s) — its content does not grow with the table, so a percentage only buys empty space beside the next column", col, strings.TrimSpace(rule))
		}
		if !strings.Contains(rule, "px") {
			t.Errorf("%s is not sized in px: %s", col, strings.TrimSpace(rule))
		}
	}

	// Narrowing the target column is only safe because it wraps. nodeCell puts
	// the node id after the hostname, so ellipsis would eat the id — the part
	// this table exists to let you copy.
	if !strings.Contains(indexHTML, "table.peers-table td.tgt-cell {") {
		t.Error("td.tgt-cell has no wrap rule; a long hostname would truncate the node id off the end of the cell")
	}
	if !strings.Contains(indexHTML, "'<td class=\"tgt-cell\">'+nodeCell(") {
		t.Error("the target cell no longer carries tgt-cell, so the wrap rule does not apply to it")
	}

	// The columns that genuinely hold variable-length values must stay
	// flexible, or the freed width has nowhere useful to go.
	if !strings.Contains(indexHTML, "table.peers-table col.c-fill { width:auto; }") {
		t.Error("c-fill is no longer auto — endpoint and notes are what absorb the width the state column gave up")
	}
}

// uiFuncSrc returns one function's source from the embedded UI, bounded by the
// start of the next top-level function so an assertion cannot be satisfied by
// something written elsewhere on the page — the same trick as enhanceTableSrc,
// generalised because the resize work is spread over several functions.
func uiFuncSrc(t *testing.T, name string) string {
	t.Helper()
	i := strings.Index(indexHTML, "function "+name+"(")
	if i < 0 {
		t.Fatalf("%s not found in indexHTML", name)
	}
	body := indexHTML[i+1:]
	if j := strings.Index(body, "\nfunction "); j >= 0 {
		body = body[:j]
	}
	return body
}

// TestColumnResizeSurvivesRerender is the column-width half of
// TestTableViewSurvivesRerender, and exists for the same reason: every table
// in the admin UI is destroyed and rebuilt by renderSection(), and Mesh >
// peers, Monitor > Mesh Peers and Bans are rebuilt every four seconds by the
// status poll. A width dragged onto a table would otherwise be undone before
// the operator let go of the next column.
//
// The width lives in state.tableView beside the sort and filter, under the key
// tableViewKey already computes — which is what makes a stale entry harmless,
// since the key folds in the column signature and a table that gained or
// renamed a column simply looks up a different one.
//
// As with the tests above, this scans the served page rather than running the
// JS, so it pins the wiring and not the behaviour.
func TestColumnResizeSurvivesRerender(t *testing.T) {
	body := enhanceTableSrc(t)

	if !strings.Contains(body, "if (view.widths && view.widths.length === ths.length) applyColWidths(table, view.widths);") {
		t.Error("enhanceTable no longer re-applies saved column widths — a dragged column would be reset by the next poll, which on the peers tables is within four seconds")
	}
	if !strings.Contains(body, "addColumnResizers(table, view);") {
		t.Error("enhanceTable no longer attaches the resize grips, so no table is resizable at all")
	}
	if !strings.Contains(uiFuncSrc(t, "addColumnResizers"), "view.widths = cur.slice();") {
		t.Error("a finished drag no longer records its widths, so there is nothing for the restore above to put back")
	}
}

// TestColumnResizeKeepsTableWidth pins the decision that a resize
// redistributes width rather than adding it.
//
// A table that can outgrow its card is a table that can put a horizontal
// scrollbar on any card in the UI; .tscroll is deliberately used on two tables
// here and not the rest. So the drag takes width from the columns to the right
// of the boundary and gives it back to them, and the total never moves —
// which is also why the last column has no grip, having nothing to its right
// to trade with.
func TestColumnResizeKeepsTableWidth(t *testing.T) {
	body := uiFuncSrc(t, "addColumnResizers")

	// Phrased against the last *visible* column since the chooser landed: with
	// a column hidden, the row's last cell is no longer the rightmost one on
	// screen.
	if !strings.Contains(body, "if (hidden[i] || i >= lastVis) return;") {
		t.Error("the last visible column now gets a grip; there is nothing to its right to take width from, so dragging it could only widen the table past its card")
	}
	if !strings.Contains(body, "let rem = -want;") {
		t.Error("the drag no longer hands the inverse of its own delta to the columns on the right — the widths would stop summing to the table and the browser would spread the error wherever it liked")
	}
	// Clamping before distributing, not during: a loop that discovers it has
	// run out of room halfway has already written some of a delta it cannot
	// finish.
	if !strings.Contains(body, "want = Math.min(want, room);") {
		t.Error("a rightward drag is no longer clamped to the room that exists, so it can ask for more width than the columns beside it can give")
	}
	if !strings.Contains(body, "want = Math.max(want, minPct[i] - start[i]);") {
		t.Error("a leftward drag is no longer clamped at the dragged column's own minimum, so a column can be dragged to zero or negative width")
	}
	if !strings.Contains(indexHTML, "const COL_MIN = 24;") {
		t.Error("COL_MIN is gone; a column has no floor to be clamped against")
	}
}

// TestColumnResizePinsLayout: under the default auto table-layout a width is a
// suggestion the browser re-weighs against cell content, so a column dragged
// narrow springs back open as soon as a long value lands in it. Fixed layout
// is what makes the drag stick, and it is applied on the first write rather
// than up front so that a table nobody has touched keeps the sizing it has
// always had.
func TestColumnResizePinsLayout(t *testing.T) {
	body := uiFuncSrc(t, "applyColWidths")

	if !strings.Contains(body, "table.style.tableLayout = 'fixed';") {
		t.Error("applyColWidths no longer switches the table to fixed layout — under auto layout the widths it writes are advisory and content will override them")
	}
	if !strings.Contains(body, "table.dataset.pinned") {
		t.Error("the pinned marker is gone; the layout switch would be redone on every pointermove, and unpinTable would have nothing to clear")
	}
	if !strings.Contains(body, "'.tscroll'") {
		t.Error("applyColWidths no longer special-cases .tscroll — those tables are width:auto, so a percentage there resolves against a width derived from the content it is about to change")
	}
	// Percentages, not pixels: a saved view has to survive being restored at a
	// different window size, and a pinned table still has to track its card.
	if !strings.Contains(body, "'%'") {
		t.Error("column widths are no longer written as percentages, so a restored view depends on the window width it was saved at")
	}
	if !strings.Contains(uiFuncSrc(t, "tableWidthPct"), "if (!(total > 0)) return null;") {
		t.Error("tableWidthPct no longer refuses a table that has never been laid out; a hidden section would pin every column to zero")
	}
}

// TestColumnResizeGuards covers the two shapes the resize must decline, and
// the colgroup it must not damage.
//
// The colspan guard is the load-bearing one. Everything downstream addresses a
// column by its header-cell index, and a spanning header cell breaks that
// correspondence for every cell after it, so a width would land on a different
// column than the edge that was dragged.
func TestColumnResizeGuards(t *testing.T) {
	head := uiFuncSrc(t, "tableHeadCells")
	if !strings.Contains(head, "c.colSpan > 1") {
		t.Error("the colspan guard is gone — on a table with a spanning header cell every width would be written onto the wrong column")
	}
	if !strings.Contains(head, "if (cells.length < 2) return null;") {
		t.Error("a single-column table is no longer excluded; its one column has nothing to trade width with")
	}

	cols := uiFuncSrc(t, "tableColEls")
	if !strings.Contains(cols, "cg.children.length !== n") {
		t.Error("tableColEls no longer checks that the existing colgroup has one col per column, so a width could be written into a col that does not correspond to it")
	}
	// peers-table and routes-table carry their widths as classes on <col>, and
	// they are the tables most worth resizing. Reusing the colgroup lets an
	// inline width override the class rule while leaving the element intact;
	// rebuilding it would drop the classes and change how the table looks
	// before anyone has dragged anything.
	if !strings.Contains(cols, "let cg = table.querySelector('colgroup');") {
		t.Error("tableColEls no longer looks for an existing colgroup — rebuilding unconditionally would discard the c-sel/c-target/rt-* classes that size peers-table and routes-table")
	}
}

// TestColumnResizeGripDoesNotSort: the grip sits inside a header cell that
// sorts on click. Both events are stopped on the grip rather than filtered in
// the sort handler, because the grip is the part that knows a click on it was
// aimed at the boundary — and a drag that also re-sorted the table on release
// would be unusable.
func TestColumnResizeGripDoesNotSort(t *testing.T) {
	body := uiFuncSrc(t, "addColumnResizers")

	if !strings.Contains(body, "grip.addEventListener('click', e => { e.stopPropagation(); });") {
		t.Error("a click on the grip now reaches the header, so finishing a drag also re-sorts the table")
	}
	if !strings.Contains(body, "e.preventDefault(); e.stopPropagation();") {
		t.Error("pointerdown on the grip no longer stops propagation or the browser's own drag/selection default")
	}
	// The only way back from a column dragged to its 24px floor.
	if !strings.Contains(body, "delete view.widths;") || !strings.Contains(body, "unpinTable(table);") {
		t.Error("double-click no longer resets the table; a column dragged to the minimum would have no way back to its original width")
	}

	// The grip must be positioned against its header cell, and the header cell
	// must be the containing block. peers-table sets overflow:hidden on th, so
	// this also has to sit wholly inside the cell.
	if !strings.Contains(indexHTML, "th.res-th { position:relative; }") {
		t.Error("th.res-th is no longer positioned, so the absolutely-positioned grip escapes to the nearest positioned ancestor")
	}
	if !strings.Contains(indexHTML, "cursor:col-resize") {
		t.Error("the grip no longer shows a col-resize cursor; a 9px invisible target with no cursor change is undiscoverable")
	}
}

// TestPinnedTableContainsItsContent covers the regression that shipped with
// the first cut of column resizing: a pinned table is table-layout:fixed, a
// fixed column does not grow to fit, and the default overflow is visible — so
// an address longer than its column painted straight over the transport
// column beside it. On Seeds every row with a full IPv6 endpoint did this at
// once, which hid the neighbouring cell's value behind it entirely.
//
// Wrapping rather than clipping, for the reason td.tgt-cell already gives: an
// ellipsis eats the end of an identifier, and the end is what distinguishes
// one address from another.
func TestPinnedTableContainsItsContent(t *testing.T) {
	const rule = "table[data-pinned] td { overflow-wrap:anywhere; }"
	if !strings.Contains(indexHTML, rule) {
		t.Fatal("the pinned-table wrap rule is gone — a column dragged narrower than its longest value overflows the cell and paints over the next column")
	}

	// Source order is load-bearing, not incidental. table.peers-table td and
	// table[data-pinned] td have identical specificity, so whichever is
	// written later wins. peers-table sets nowrap + ellipsis deliberately and
	// is already fixed-layout; moving the generic rule below it would take
	// that away without anything looking wrong at the point of the edit.
	pinned := strings.Index(indexHTML, rule)
	for _, later := range []string{
		"table.peers-table td, table.peers-table th {",
		"table.routes-table td.cidr-cell {",
	} {
		i := strings.Index(indexHTML, later)
		if i < 0 {
			t.Errorf("%q is gone; the ordering this rule depends on cannot be checked", later)
			continue
		}
		if pinned > i {
			t.Errorf("the generic pinned wrap rule is now written after %q — at equal specificity it wins, and that table loses the overflow behaviour it sets on purpose", later)
		}
	}

	// A form control does not wrap, and cell-edit has a 90px floor of its own
	// that a dragged column can easily go under.
	if !strings.Contains(indexHTML, "table[data-pinned] input.cell-edit { min-width:0; }") {
		t.Error("an inline editor in a pinned table can no longer shrink below 90px, so opening one in a narrower column overflows it")
	}
}

// TestHeaderLabelsAreNotReformatted covers the second regression from the
// resize work: resizing one column must not change how any other column's
// heading is laid out.
//
// Two separate causes, both real. The wrap rule was applied to th as well as
// td, so a squeezed header broke mid-label — "name" rendered as four stacked
// letters. And the floor a column could be dragged to was a flat 24px, which
// is narrower than most labels, so a drag on one column could squeeze its
// neighbours below the width their own headings needed.
func TestHeaderLabelsAreNotReformatted(t *testing.T) {
	// A header is a fixed short label, not content. It never wraps.
	if strings.Contains(indexHTML, "table[data-pinned] td, table[data-pinned] th { overflow-wrap:anywhere; }") {
		t.Error("the wrap rule covers th again — a squeezed header will break mid-label and stack one letter per line")
	}
	if !strings.Contains(indexHTML, "table[data-pinned] td { overflow-wrap:anywhere; }") {
		t.Error("data cells no longer wrap, so a long address overflows its column again")
	}
	if !strings.Contains(indexHTML, "table[data-pinned] th { white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }") {
		t.Error("pinned headers are no longer held to one line; the ellipsis backstop for a restored-too-narrow width is gone with it")
	}

	// The floor has to be per column and derived from the label, or a drag on
	// one column reformats the heading of another.
	body := uiFuncSrc(t, "addColumnResizers")
	if !strings.Contains(body, "hidden[j] ? 0 : Math.max(COL_MIN, headerMinPx(c)) / totalPx * 100") {
		t.Error("the per-column floor is gone — with one shared minimum, widening any column can squeeze its neighbours past the width their own headings need")
	}
	for _, use := range []string{"start[j] - minPct[j]", "minPct[i] - start[i]"} {
		if !strings.Contains(body, use) {
			t.Errorf("the drag math no longer indexes the floor per column (%s); every column would be clamped against whichever single value survived", use)
		}
	}

	min := uiFuncSrc(t, "headerMinPx")
	// Measured off-screen on purpose: a squeezed header has already wrapped or
	// clipped, so reading its own box back would return the damaged width and
	// ratify it.
	if !strings.Contains(min, "document.createElement('span')") || !strings.Contains(min, "document.body.removeChild(probe)") {
		t.Error("headerMinPx no longer measures against a probe it cleans up — measuring the cell itself would return the squeezed width, not the width the label needs")
	}
	if !strings.Contains(min, "whiteSpace = 'pre'") {
		t.Error("the probe can wrap, so it measures a broken label rather than the single line the floor is meant to guarantee")
	}
	if !strings.Contains(min, "paddingLeft") || !strings.Contains(min, "paddingRight") {
		t.Error("headerMinPx ignores cell padding, so the floor is 20px short and the label still clips")
	}
	if !strings.Contains(min, "sortable-th") {
		t.Error("headerMinPx no longer reserves room for the sort arrow; a sorted column's label loses a character to it")
	}
}

// TestColumnChooserWiring pins the parts of the show/hide gear that a later
// edit could remove without anything obvious breaking at the point of the
// edit.
func TestColumnChooserWiring(t *testing.T) {
	body := uiFuncSrc(t, "addColumnChooser")

	// Only labelled columns are offered. A blank heading is a checkbox or
	// action column: there is nothing to name it in the list, and hiding the
	// select column would take the row selection out from under the toolbar
	// buttons that act on it.
	if !strings.Contains(body, "if (c.textContent.trim()) hideable.push(i);") {
		t.Error("the chooser no longer filters to labelled columns, so the selection column appears as a blank entry and can be hidden")
	}
	if !strings.Contains(body, "if (hideable.length < 2) return;") {
		t.Error("a table with one labelled column now gets a gear that cannot usefully do anything")
	}
	// A table with every column hidden shows nothing and cannot be recovered,
	// because the gear lives in a header cell that no longer exists.
	if !strings.Contains(body, "boxes[j].disabled = only;") {
		t.Error("the last visible column can be unticked again — the table would empty itself and take the gear with it")
	}
	// Ticking a column must not also sort the table underneath the menu.
	if !strings.Contains(body, "menu.addEventListener('click', e => { e.stopPropagation(); });") {
		t.Error("a click inside the menu reaches the header again, so choosing a column also re-sorts the table")
	}
	// See startPolling: the four-second poll skips a re-render while an INPUT
	// holds focus, which is the only thing stopping the peers pages from
	// tearing the menu down while it is open.
	if !strings.Contains(body, "first.focus();") {
		t.Error("the menu no longer parks focus in a checkbox, so the status poll will re-render the table out from under an open chooser")
	}
	if !strings.Contains(body, "delete view.widths;") || !strings.Contains(body, "unpinTable(table);") {
		t.Error("a visibility change no longer clears the dragged widths — the stored percentages describe a set of columns that no longer exists")
	}

	if !strings.Contains(enhanceTableSrc(t), "addColumnChooser(table, view);") {
		t.Error("enhanceTable no longer adds the chooser, so no table has a gear")
	}
}

// TestColumnVisibilitySurvivesRerender: hidden columns live in state.tableView
// beside the sort, filter and widths, and for the same reason — the table is
// destroyed and rebuilt by renderSection, every four seconds on the peers
// pages. A column hidden by hand would otherwise reappear on the next poll.
func TestColumnVisibilitySurvivesRerender(t *testing.T) {
	body := enhanceTableSrc(t)

	if !strings.Contains(body, "if (view.hidden && view.hidden.length === ths.length) applyColHidden(table, view.hidden);") {
		t.Error("enhanceTable no longer re-applies hidden columns; a hidden column comes back on the next re-render")
	}
	// Order matters: both the grips and the width application read the hidden
	// set, so it has to be on the table before either runs.
	hid := strings.Index(body, "applyColHidden(table, view.hidden);")
	for _, after := range []string{"applyColWidths(table, view.widths);", "addColumnResizers(table, view);"} {
		i := strings.Index(body, after)
		if i < 0 {
			t.Errorf("%q is gone; the ordering cannot be checked", after)
			continue
		}
		if hid > i {
			t.Errorf("visibility is restored after %q, which reads it — the grips would stop at the wrong column and hidden columns would be given width", after)
		}
	}

	// The <col> has to be hidden with the cells. Under table-layout:fixed the
	// column's width comes from the col, so hiding only the cells empties the
	// column but keeps the gap it occupied.
	if !strings.Contains(uiFuncSrc(t, "applyColHidden"), "cg.children.length === hidden.length") {
		t.Error("applyColHidden no longer hides the matching <col>, so a hidden column leaves its width behind on a resized table")
	}

	// The resize math has to agree with the chooser about which columns exist.
	res := uiFuncSrc(t, "addColumnResizers")
	if !strings.Contains(res, "if (hidden[i] || i >= lastVis) return;") {
		t.Error("grips no longer stop at the last visible column; hiding the rightmost column leaves a grip with nothing beyond it to trade width with")
	}
	if !strings.Contains(res, "hidden[j] ? 0 : Math.max(COL_MIN, headerMinPx(c))") {
		t.Error("a hidden column still claims its label's width as a floor, holding back space for a column that is not on screen")
	}
	if !strings.Contains(res, "if (hidden[j]){ cur[j] = start[j]; continue; }") {
		t.Error("the distribution loop no longer skips hidden columns — a leftward drag would hand its surplus to one and park it off screen")
	}
}

// TestColumnMenuEscapesItsContainer: the chooser panel is parented to <body>
// and positioned in viewport coordinates, because an absolutely-positioned
// menu is clipped by any ancestor that scrolls.
//
// The first cut nested it in the gear and exempted th.gear-th from
// overflow:hidden, which was necessary and nowhere near sufficient — the
// Networks table sits in .tscroll, and overflow-x:auto makes the computed
// overflow-y auto too, so the container clipped the panel at the bottom of the
// table. On a one-row table that left a single entry visible. There is no set
// of ancestor exemptions that fixes this in general; the fix is to have no
// clipping ancestor at all.
func TestColumnMenuEscapesItsContainer(t *testing.T) {
	if !strings.Contains(indexHTML, ".colmenu { display:none; position:fixed;") {
		t.Error("the chooser panel is not position:fixed — parented to <body> it would be positioned against the page, not the viewport")
	}
	if strings.Contains(indexHTML, ".colmenu { display:none; position:absolute;") {
		t.Error("the panel is absolutely positioned again, so any scrolling ancestor (.tscroll on Networks) clips it")
	}

	body := uiFuncSrc(t, "addColumnChooser")
	if !strings.Contains(body, "document.body.appendChild(menu);") {
		t.Error("the panel is no longer parented to <body> on open; nested in the table it is clipped by .tscroll")
	}
	if strings.Contains(body, "gear.appendChild(menu);") {
		t.Error("the panel is nested in the gear again — that is the arrangement .tscroll clipped to a single row")
	}
	// Parented to the body, it outlives its table and follows nothing.
	if !strings.Contains(uiFuncSrc(t, "closeColMenu"), "m.parentNode.removeChild(m)") {
		t.Error("closing no longer detaches the panel, so every open leaves another one on <body>")
	}
	if !strings.Contains(body, "if (openColMenu && !document.contains(openColMenu.table)) closeColMenu();") {
		t.Error("a panel whose table was re-rendered is left floating over a header that no longer exists")
	}
	if !strings.Contains(body, "placeMenu();") {
		t.Error("the panel is never positioned against its gear")
	}
	// A scroll inside .tscroll does not bubble, so the listener has to capture
	// or the panel stays put while its anchor slides away.
	if !strings.Contains(body, "window.addEventListener('scroll', placeMenu, true);") {
		t.Error("the panel does not re-position on scroll, so it detaches visually from the gear it points at")
	}
	if !strings.Contains(body, "window.innerHeight") {
		t.Error("the panel no longer flips above the gear when there is no room below it")
	}

	// The gear overlays the right edge of the last visible header cell, so
	// that cell has to reserve the room or the glyph sits on the label.
	if !strings.Contains(indexHTML, "table th.gear-th { position:relative; overflow:visible; padding-right:26px; }") {
		t.Error("the gear column no longer reserves room for the gear, so it overlaps the heading text")
	}
}
