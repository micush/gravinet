package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// TestNetAddRowMatchesHeaderColumns guards against reintroducing "when i
// create a new mesh network, the fields don't line up".
//
// netAddRow builds the inline create row for the Networks table as a literal
// run of <td>s. Nothing ties that run to the <th> run in the header, so the
// two drift independently, and there are two distinct ways for them to end up
// disagreeing:
//
//   - A missing cell. The row had no cell for the mesh column, so the browser
//     packed everything after state one column left: the subnet4 input sat
//     under the mesh header and the subnet6 input under subnet4. Every field
//     still worked and still submitted the right value — it was labelled by
//     the wrong header, which is worse than a broken field, because nothing
//     looks broken.
//   - A stale colspan. The trailing button cell was written colspan="3" when
//     the header had 9 columns. The header has since grown to 13, and the
//     literal did not, so the row covered 9 of them and mtu, peers, seeds and
//     notes had no cell at all.
//
// Neither is visible from reading netAddRow on its own; both need the header
// in front of you. So this test puts them side by side: it reads the real
// column order out of the header, walks the create row's cells, and checks
// that each input lands under the header it is named for and that the row
// covers the full width.
//
// Like TestNoStandaloneTrOrTdViaInnerHTML, this scans the served page as text
// rather than running the JS — this package has no JS runtime in its test
// suite.
func TestNetAddRowMatchesHeaderColumns(t *testing.T) {
	cols := networksHeaderColumns(t)
	if len(cols) < 9 {
		t.Fatalf("networks header parsed as only %d columns (%v) — the parse is wrong, not the page", len(cols), cols)
	}

	row := between(t, indexHTML, "function netAddRow(table){", "if (!insertNewRow")

	// Where each named input sits, in header-column terms.
	spanRe := regexp.MustCompile(`^ colspan="'\+Math\.max\(1, cols-(\d+)\)`)
	at := map[string]string{}
	covered := 0
	for _, cell := range strings.Split(row, "<td")[1:] {
		width := 1
		if m := spanRe.FindStringSubmatch(cell); m != nil {
			// A span derived from the live header: cols-N covers everything
			// from here to the end, so long as exactly N fixed cells precede
			// it. That is checked below via the total.
			width = len(cols) - atoi(t, m[1])
		} else if strings.Contains(firstN(cell, 60), "colspan=") {
			t.Errorf("netAddRow has a literal colspan: %q. Derive it from table.rows[0].cells.length "+
				"instead — a number is only correct until the next column is added, which is how "+
				"mtu/peers/seeds/notes lost their cell.", strings.TrimSpace(firstN(cell, 60)))
		}
		for _, cls := range []string{"ne-name", "ne-s4", "ne-s6", "ne-save"} {
			if strings.Contains(firstN(cell, 200), cls) {
				if covered < len(cols) {
					at[cls] = cols[covered]
				} else {
					at[cls] = "past the end of the row"
				}
			}
		}
		covered += width
	}

	// Each input under the header that names it. ne-save is deliberately not
	// checked: the buttons live in the trailing span and may sit under any of
	// the columns it covers.
	for _, want := range []struct{ cls, header string }{
		{"ne-name", "name"},
		{"ne-s4", "subnet4"},
		{"ne-s6", "subnet6"},
	} {
		got, ok := at[want.cls]
		if !ok {
			t.Errorf("no cell in netAddRow contains %q — the input was renamed or removed; update this test with it", want.cls)
			continue
		}
		if got != want.header {
			t.Errorf("netAddRow's %s input sits under the %q column, want %q. The row is missing a cell "+
				"before it (or has one too many): every field after the gap is labelled by the wrong header.",
				want.cls, got, want.header)
		}
	}

	if covered != len(cols) {
		t.Errorf("netAddRow covers %d columns, header has %d. The %v column(s) have no cell in the create row.",
			covered, len(cols), cols[min(covered, len(cols)):])
	}
}

// networksHeaderColumns returns the Networks table's header labels in order,
// with the leading checkbox column reported as "selcol".
func networksHeaderColumns(t *testing.T) []string {
	t.Helper()
	anchor := strings.Index(indexHTML, ">subnet4</th>")
	if anchor < 0 {
		t.Fatal(`no ">subnet4</th>" in indexHTML — the Networks header was restructured; this test needs updating`)
	}
	start := strings.LastIndex(indexHTML[:anchor], "<table><tr>")
	end := strings.Index(indexHTML[anchor:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatal("could not bound the Networks header row in indexHTML")
	}
	var out []string
	for _, th := range strings.Split(indexHTML[start:anchor+end], "<th")[1:] {
		i := strings.Index(th, "</th>")
		if i < 0 {
			continue
		}
		label := th[:i]
		// Skip the attributes (including title text, which contains '>'
		// nowhere but is long) and take the cell's text.
		if j := strings.LastIndex(label, ">"); j >= 0 {
			label = label[j+1:]
		}
		if label == "" {
			label = "selcol"
		}
		out = append(out, label)
	}
	return out
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("not a number: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
