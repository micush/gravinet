package webadmin

import (
	"strings"
	"testing"
)

// The header node picker's filter box must live at the TOP OF THE DROPDOWN LIST,
// not beside it. A native <select> can't do that — its option list is an OS-drawn
// popup that no markup can be placed inside — which is exactly why the filter
// used to sit next to the picker as a separate header control. So the picker is a
// hand-rolled listbox, and these are the invariants that keep it one.
func TestPeerPickerIsNotANativeSelect(t *testing.T) {
	if strings.Contains(indexHTML, `$('<select id="peerSel"`) {
		t.Error("peer picker is a native <select> again; its filter can then only sit beside it, never at the top of the list")
	}
	if strings.Contains(indexHTML, "peer-filter") {
		t.Error("the old beside-the-select .peer-filter input is back; the filter belongs inside the dropdown (.ss-filter-row)")
	}
	for _, want := range []string{
		"function buildPeerPicker(", // the hand-rolled listbox
		"buildListPicker(",          // built on the shared component
		`class="ss-filter-row"`,     // the filter's row, inside the list
		`class="ss-list`,            // the popup it's pinned to the top of
		`role="listbox"`,            // it stands in for a <select>, so say so
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("peer picker markup missing %q", want)
		}
	}
}

// The filter row is built as the list's first child, ahead of the options
// container — that ordering *is* the feature ("at the top of the list").
func TestPeerPickerFilterIsFirstInTheList(t *testing.T) {
	appendFilter := strings.Index(indexHTML, "list.appendChild(filterRow);")
	appendOpts := strings.Index(indexHTML, "list.appendChild(optBox);")
	if appendFilter < 0 || appendOpts < 0 {
		t.Fatal("peer picker no longer appends a filter row and an options box to its list")
	}
	if appendFilter > appendOpts {
		t.Error("filter row is appended after the options, so it renders below them; it must be the top row of the list")
	}
}

// indexHTML is a Go raw string literal: no escape processing. A "\u25be" written
// in it reaches the browser as those literal characters and renders as visible
// text in the button, which is exactly what happened the first time round.
func TestUINoRawUnicodeEscapes(t *testing.T) {
	if strings.Contains(indexHTML, `\u25be`) {
		t.Error(`indexHTML contains a literal \u escape; a Go raw string doesn't process it, so it renders as text — use an HTML entity (&#9662;)`)
	}
}

// The speedtest's source/target pickers were the last native <select>s with a
// filter input parked beside them — the pattern the header picker was rebuilt to
// get rid of. They now share buildListPicker, so their filters sit at the top of
// their own dropdowns too.
func TestSpeedtestPickersAreListboxes(t *testing.T) {
	body := speedtestFn(t)
	if strings.Contains(body, "$('<select") {
		t.Error("infoSpeedtest builds a native <select> again; its filter could then only sit beside it")
	}
	if strings.Contains(body, `placeholder="filter…" style=`) {
		t.Error("the old inline-styled filter input is back beside a speedtest picker")
	}
	for _, want := range []string{
		"pickA = buildListPicker(", // shares the header's listbox
		"pickB = buildListPicker(",
		"const exclude = ",   // a node can't be both endpoints
		"disabled: it.value", // exclusion expressed as a disabled option
	} {
		if !strings.Contains(body, want) {
			t.Errorf("infoSpeedtest missing %q", want)
		}
	}
}

// buildListPicker must actually render a disabled option as inert, not merely
// gray: it's what stops the speedtest running a node against itself.
func TestListPickerDisablesOptions(t *testing.T) {
	for _, want := range []string{
		"if (it.disabled) return;",            // pick() refuses
		`if (!it.disabled){`,                  // no handlers bound to a disabled option
		"if (!shown[selIdx].disabled) break;", // keyboard steps over it
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("buildListPicker missing disabled-option handling: %q", want)
		}
	}
}

// A backtick inside indexHTML would end the Go raw string literal mid-file. That
// can't reach production (it wouldn't compile) but it's an easy trap to fall into
// when writing JS comments in here, so name it explicitly rather than leaving the
// next person to decode a syntax error hundreds of lines away.
func TestUIRawStringHasNoStrayBackticks(t *testing.T) {
	if strings.Contains(indexHTML, "`") {
		t.Error("indexHTML contains a backtick, which would terminate the Go raw string literal")
	}
}

// speedtestFn returns the source of infoSpeedtest, so the assertions above are
// scoped to that function rather than matching something elsewhere in the file.
func speedtestFn(t *testing.T) string {
	t.Helper()
	const start = "function infoSpeedtest(c){"
	i := strings.Index(indexHTML, start)
	if i < 0 {
		t.Fatal("infoSpeedtest not found in indexHTML")
	}
	rest := indexHTML[i:]
	// Functions here are top-level, so the next one begins at a column-0 "function".
	if j := strings.Index(rest[1:], "\nfunction "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}
