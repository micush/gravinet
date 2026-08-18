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
