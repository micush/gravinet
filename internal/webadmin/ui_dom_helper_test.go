package webadmin

import (
	"strings"
	"testing"
)

// TestNoStandaloneTrOrTdViaInnerHTML guards against reintroducing the bug
// behind "BGP editor renders blank / hangs on Checking… forever with no
// error": the embedded UI's $() helper parses its argument as innerHTML on a
// plain <div>, and every browser's HTML parser silently drops a bare <tr> or
// <td> there — they're only valid inside a real <table> context — so
// $('<tr></tr>') and $('<td></td>') both evaluate to null. The very next
// .appendChild on that null then throws, and since this happens inside a
// synchronous DOM-building call with nothing above it to catch the
// exception, the render aborts before the finished element is ever attached
// — leaving whatever was on screen before the call (a spinner, a "Checking…"
// label, the previous card) exactly as it was, no visible error at all.
//
// This exact failure mode only shows up once real data reaches the affected
// table (an imported or manually-added BGP neighbor row) — a brand-new,
// empty config never exercises it — which is why it can ship unnoticed.
//
// The fix is to build <tr>/<td> nodes with document.createElement instead,
// which has no such context requirement. This test scans the served page for
// the broken pattern directly, rather than running the JS (this package has
// no JS runtime dependency in its test suite), so it fails loudly if the
// pattern — in the BGP editor or in any future table built the same way —
// ever comes back.
func TestNoStandaloneTrOrTdViaInnerHTML(t *testing.T) {
	bad := []string{`$('<tr>`, `$('<tr></tr>')`, `$("<tr>`, `$('<td>`, `$('<td></td>')`, `$("<td>`}
	for _, pat := range bad {
		if idx := strings.Index(indexHTML, pat); idx >= 0 {
			t.Errorf("found %q in indexHTML at byte offset %d — a standalone <tr>/<td> built via the $() "+
				"innerHTML helper parses to null in every real browser (only valid inside a <table>), and the "+
				"next .appendChild on it throws, silently aborting the render. Use document.createElement('tr'/'td') instead.",
				pat, idx)
		}
	}
}
