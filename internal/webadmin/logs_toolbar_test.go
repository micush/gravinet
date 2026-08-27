package webadmin

import (
	"strings"
	"testing"
)

// tshoot is set apart from the log controls and sits at the far end of the bar.
//
// Refresh, Download and Clear all act on this node's log file. tshoot collects
// a whole diagnostic bundle and may fan out across every reachable mesh peer,
// which is a different kind of thing to be doing — it was sitting fourth in a
// row of four, reading as another log control.
//
// Two halves, and both are needed. right:true supplies margin-left:auto, and
// being last in _rowButtons is what makes that push the button alone: the
// class starts a right-aligned *group*, so anything after it in the array
// travels with it. Reordering the array would silently drag Clear along, which
// is the regression this pins.
func TestLogsTshootIsSetApartAtTheEnd(t *testing.T) {
	bar := logsRowButtons(t)

	if !strings.Contains(bar, "{ label:'tshoot', cls:'', right:true,") {
		t.Error("the logs tshoot button is not right-aligned; it sits in the row of log controls as though it were one")
	}

	iTshoot := strings.Index(bar, "label:'tshoot'")
	if iTshoot < 0 {
		t.Fatal("no tshoot button in the logs toolbar")
	}
	for _, other := range []string{"label:'Refresh'", "label:'Download'", "label:'Clear'"} {
		i := strings.Index(bar, other)
		if i < 0 {
			t.Errorf("the logs toolbar no longer has %s", other)
			continue
		}
		if i > iTshoot {
			t.Errorf("%s comes after tshoot in _rowButtons, so it is inside tshoot's right-aligned group and gets pushed to the end with it", other)
		}
	}
}

// Nothing else in the strip is right-aligned. Two right-aligned buttons in one
// toolbar means two auto margins splitting the free space between them, which
// is not the layout anyone intended and looks like a bug rather than a choice.
func TestLogsHasExactlyOneRightAlignedButton(t *testing.T) {
	bar := logsRowButtons(t)
	if n := strings.Count(bar, "right:true"); n != 1 {
		t.Errorf("the logs toolbar has %d right-aligned buttons, want exactly 1", n)
	}
}

// logsRowButtons returns the _rowButtons array literal from secLogs, with its
// comment lines stripped — the comments discuss right:true by name, and a test
// that counted occurrences in prose would be counting the explanation as well
// as the thing explained.
func logsRowButtons(t *testing.T) string {
	t.Helper()
	i := strings.Index(indexHTML, "function secLogs(")
	if i < 0 {
		t.Fatal("secLogs not found")
	}
	body := indexHTML[i:]
	j := strings.Index(body, "table._rowButtons = [")
	if j < 0 {
		t.Fatal("the logs toolbar no longer declares _rowButtons")
	}
	body = body[j:]
	k := strings.Index(body, "\n  ];")
	if k < 0 {
		t.Fatal("could not find the end of the logs _rowButtons array")
	}
	var kept []string
	for _, ln := range strings.Split(body[:k], "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
