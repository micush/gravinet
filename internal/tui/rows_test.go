package tui

import (
	"strings"
	"testing"
)

// TestLayoutTableStructuralInvariant pins the exact shape rowLineOffset
// depends on without re-implementing: header, rule, one line per row, in
// row order, nothing else. If this ever changes, rowLineOffset's "+2 +
// row.rowIdx" arithmetic has to change with it — this test is what makes
// that a loud, obvious failure instead of a cursor that quietly highlights
// the wrong line.
func TestLayoutTableStructuralInvariant(t *testing.T) {
	for _, n := range []int{0, 1, 5} {
		tb := table{head: []string{"a", "b"}}
		for i := 0; i < n; i++ {
			tb.rows = append(tb.rows, tableRow{cells: []string{"x", "y"}})
		}
		got := layoutTable(tb, testCtx(40))
		if want := 2 + n; len(got) != want {
			t.Errorf("%d rows: layoutTable produced %d lines, want %d", n, len(got), want)
		}
	}
	// The invariant has to hold for a selectable table too — the gutter
	// changes column widths, not the line count.
	tb := table{selectKey: "x", ids: []string{"a", "b", "c"}, head: []string{"h"},
		rows: []tableRow{{cells: []string{"1"}}, {cells: []string{"2"}}, {cells: []string{"3"}}}}
	if got := layoutTable(tb, testCtx(40)); len(got) != 5 {
		t.Errorf("selectable table: got %d lines, want 5", len(got))
	}
}

func TestFlattenSelectableOrderAndSkipsReadOnlyTables(t *testing.T) {
	cards := []card{
		{title: "one", items: []item{
			table{selectKey: "nets", ids: []string{"a", "b"}, head: []string{"h"},
				rows: []tableRow{{cells: []string{"1"}}, {cells: []string{"2"}}}},
			table{head: []string{"h"}, rows: []tableRow{{cells: []string{"not selectable"}}}},
		}},
		{title: "two", items: []item{
			table{selectKey: "keys", ids: []string{"c"}, head: []string{"h"},
				rows: []tableRow{{cells: []string{"3"}}}},
		}},
	}
	got := flattenSelectable(cards)
	want := []selRow{
		{tableKey: "nets", id: "a", cardIdx: 0, itemIdx: 0, rowIdx: 0},
		{tableKey: "nets", id: "b", cardIdx: 0, itemIdx: 0, rowIdx: 1},
		{tableKey: "keys", id: "c", cardIdx: 1, itemIdx: 0, rowIdx: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFindSelRow(t *testing.T) {
	rows := []selRow{{tableKey: "nets", id: "a"}, {tableKey: "nets", id: "b"}}
	if i, ok := findSelRow(rows, "nets", "b"); !ok || i != 1 {
		t.Errorf("findSelRow(b) = %d,%v", i, ok)
	}
	if _, ok := findSelRow(rows, "nets", "missing"); ok {
		t.Error("found a row that isn't there")
	}
	if _, ok := findSelRow(rows, "", ""); ok {
		t.Error("an empty (tableKey, id) pair — meaning nothing selected — should never match")
	}
}

// TestRowLineOffsetMatchesTheRealRenderer is the important one: it builds a
// page with several cards and several selectable rows, then for every one of
// them, actually renders the full page and independently locates that row's
// line by looking for its unique marker text — and checks that
// rowLineOffset's answer, which never looks at rendered output at all,
// lands on the exact same line. If the two ever disagree, the viewport would
// scroll to the wrong place while the highlight sat somewhere else — the
// worst failure mode this file exists to prevent, so this test drives both
// paths independently rather than trusting rowLineOffset's own reasoning
// about itself.
func TestRowLineOffsetMatchesTheRealRenderer(t *testing.T) {
	cards := []card{
		{title: "networks", items: []item{
			kv{rows: []kvRow{{"a", "1", ""}, {"b", "2", ""}}},
			table{selectKey: "nets", ids: []string{"corp", "lab"}, head: []string{"name"},
				rows: []tableRow{{cells: []string{"MARK-corp"}}, {cells: []string{"MARK-lab"}}}},
		}},
		{title: "spacer", items: []item{
			para{text: "some prose that takes a little room before the next table, long enough that at a narrow width it wraps onto more than one line, which is exactly the case that would break a line-counting formula that didn't account for wrapping"},
		}},
		{title: "keys", items: []item{
			table{selectKey: "keys", ids: []string{"slot0"}, head: []string{"slot"},
				rows: []tableRow{{cells: []string{"MARK-slot0"}}}},
		}},
	}

	for _, width := range []int{40, 100} {
		ctx := testCtx(width)
		rendered := layout(cards, ctx)
		rows := flattenSelectable(cards)
		if len(rows) != 3 {
			t.Fatalf("width %d: expected 3 selectable rows, got %d", width, len(rows))
		}
		for _, r := range rows {
			want := findLineContaining(t, rendered, "MARK-"+r.id)
			got := rowLineOffset(cards, ctx, r)
			if got != want {
				t.Errorf("width %d, row %s: rowLineOffset = %d, actual rendered line = %d", width, r.id, got, want)
			}
		}
	}
}

// findLineContaining locates the one line in lines whose text contains
// marker, failing the test if it's missing or ambiguous.
func findLineContaining(t *testing.T, lines []line, marker string) int {
	t.Helper()
	found := -1
	for i, l := range lines {
		if strings.Contains(l.text(), marker) {
			if found >= 0 {
				t.Fatalf("marker %q appears on more than one line (%d and %d)", marker, found, i)
			}
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("marker %q not found in rendered output", marker)
	}
	return found
}
