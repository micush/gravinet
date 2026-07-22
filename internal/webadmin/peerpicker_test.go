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

// TestHeaderPeerPickerHasFixedWidth pins the fix for a reported bug: the
// header's peer picker had no width of its own, so it visibly resized on
// every pick — "This node" versus a peer's (usually longer, and
// differently-long peer to peer) hostname reflowed the whole top bar. The
// fix is scoped to #peerSel specifically, not the base .peer-sel class, so
// it must not touch .peer-sel-sm (the speedtest pickers) at all — nothing
// about their sizing was reported as broken, and a global width change to
// the shared class risks breaking their tighter toolbar layout instead.
func TestHeaderPeerPickerHasFixedWidth(t *testing.T) {
	if !strings.Contains(indexHTML, "#peerSel { width:") {
		t.Error("no fixed width rule for #peerSel — the header peer picker can still resize on every pick")
	}
	if !strings.Contains(indexHTML, "#peerSel .peer-sel-label") || !strings.Contains(indexHTML, "text-overflow:ellipsis") {
		t.Error("#peerSel's label isn't set to truncate — a long hostname would overflow the fixed-width button instead of ellipsizing")
	}
	// The fix must not be a bare .peer-sel rule: that class is shared with
	// .peer-sel-sm (the speedtest pickers), and widening it there was never
	// asked for and isn't proven safe for that layout.
	if strings.Contains(indexHTML, "\n  .peer-sel { ") {
		body := indexHTML[strings.Index(indexHTML, "\n  .peer-sel { "):]
		body = body[:strings.IndexByte(body, '\n')+1]
		if strings.Contains(body, "width:") {
			t.Error(".peer-sel itself gained a width rule — this should be scoped to #peerSel, not every picker sharing the base class")
		}
	}
}

// TestSpeedtestPickersHaveFixedWidth pins the fix for the same bug
// TestHeaderPeerPickerHasFixedWidth fixed for the header, reported
// separately for the speedtest's two pickers: no width of their own meant
// they resized on every pick, since "This node (local)" and a peer's
// hostname are rarely the same length. v566 deliberately left .peer-sel-sm
// alone when fixing #peerSel — nothing about these two was reported broken
// at the time, and a bare width on the shared .peer-sel base class would
// have widened them without being asked. Now it has been reported, fixed by
// widening .peer-sel-sm specifically (the class buildListPicker's
// compact:true gives these two, and only these two — see
// TestSpeedtestPickersAreListboxes's own comment on there being exactly
// three buildListPicker call sites total), not the shared base class.
func TestSpeedtestPickersHaveFixedWidth(t *testing.T) {
	if !strings.Contains(indexHTML, ".peer-sel-sm { width:") {
		t.Error("no fixed width rule for .peer-sel-sm — the speedtest pickers can still resize on every pick")
	}
	if !strings.Contains(indexHTML, ".peer-sel-sm .peer-sel-label") || !strings.Contains(indexHTML, "text-overflow:ellipsis") {
		t.Error(".peer-sel-sm's label isn't set to truncate — a long hostname would overflow the fixed-width button instead of ellipsizing")
	}
}

// "function "+name+"(", up to (not including) the next top-level function —
// the same bounded-extraction approach speedtestFn already used, generalized
// so other tests can scope an assertion to one function's body instead of
// matching (or, for a negative assertion, risking a false pass against)
// anywhere else in indexHTML.
func funcSource(t *testing.T, name string) string {
	t.Helper()
	start := "function " + name + "("
	i := strings.Index(indexHTML, start)
	if i < 0 {
		t.Fatalf("%s not found in indexHTML", start)
	}
	rest := indexHTML[i:]
	if j := strings.Index(rest[1:], "\nfunction "); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

// TestSpeedtestPpsIsOneCombinedChart pins the corrected layout: download and
// upload packets/sec share one third chart (speedPpsCard), not a fourth
// chart bolted onto each of the Download/Upload cards individually. The
// first version of this did exactly that — a small pps sub-chart appended
// inside speedGraph, once per direction — which is the wrong shape this
// TestSpeedtestPpsIsOneCombinedChart pins the corrected layout: download and
// upload packets/sec share one third chart (speedPpsCard), not a fourth
// chart bolted onto each of the Download/Upload cards individually. The
// first version of this did exactly that — a small pps sub-chart appended
// inside speedGraph, once per direction — which is the wrong shape this
// test exists to keep from coming back.
func TestSpeedtestPpsIsOneCombinedChart(t *testing.T) {
	rsr := funcSource(t, "renderSpeedResult")
	if !strings.Contains(rsr, "speedPpsCard(") {
		t.Error("renderSpeedResult doesn't build a speedPpsCard — packets/sec has no combined chart")
	}
	// Exactly two speedGraph calls (Download, Upload) and nothing else
	// building a per-direction pps chart alongside them.
	if n := strings.Count(rsr, "speedGraph("); n != 2 {
		t.Errorf("renderSpeedResult calls speedGraph %d times, want exactly 2 (Download, Upload)", n)
	}

	// speedGraph itself must be back to a single chart — no packet_samples
	// reference in its own body, which is what would mean a pps chart is
	// still being embedded per-direction.
	sg := funcSource(t, "speedGraph")
	if strings.Contains(sg, "packet_samples") {
		t.Error("speedGraph still references packet_samples — a per-direction pps chart is back instead of one combined chart")
	}

	// speedPpsCard must plot both directions on the one chart it builds,
	// using each direction's own established color (matching the Download/
	// Upload cards above it) and passing both series into a single
	// speedComboChartSVG call, not one call each.
	spc := funcSource(t, "speedPpsCard")
	if !strings.Contains(spc, "var(--acc)") || !strings.Contains(spc, "#e0883b") {
		t.Error("speedPpsCard doesn't use both the Download (var(--acc)) and Upload (#e0883b) colors")
	}
	if n := strings.Count(spc, "speedComboChartSVG("); n != 1 {
		t.Errorf("speedPpsCard calls speedComboChartSVG %d times, want exactly 1 (both series on one chart)", n)
	}
	if !strings.Contains(spc, "down.packet_samples") || !strings.Contains(spc, "up.packet_samples") {
		t.Error("speedPpsCard doesn't feed both down and up's packet_samples into the chart")
	}
}

// TestSpeedtestChartsHaveHoverCrosshair pins the hover crosshair added to
// all three speedtest charts: attachSpeedChartHover wired into both
// speedGraph and speedPpsCard, sourced from the same hoverScaffold markup
// the Metrics dashboard's own crosshair (attachChartHover/chartSVG) uses —
// not a second, parallel implementation of the capture-rect/dot/line
// markup that could drift from it. Also pins the one deliberate behavioral
// difference from attachChartHover: its tooltip header must NOT be
// clockOf(t) (a wall-clock HH:MM:SS, meaningless for a speedtest's elapsed-
// time axis) — it formats t directly as seconds instead.
func TestSpeedtestChartsHaveHoverCrosshair(t *testing.T) {
	if !strings.Contains(indexHTML, "function attachSpeedChartHover(") {
		t.Fatal("attachSpeedChartHover is missing")
	}
	if !strings.Contains(indexHTML, "function hoverScaffold(") {
		t.Fatal("hoverScaffold helper is missing")
	}

	sg := funcSource(t, "speedGraph")
	if !strings.Contains(sg, "attachSpeedChartHover(") {
		t.Error("speedGraph doesn't wire up attachSpeedChartHover — no crosshair on the Download/Upload charts")
	}
	spc := funcSource(t, "speedPpsCard")
	if !strings.Contains(spc, "attachSpeedChartHover(") {
		t.Error("speedPpsCard doesn't wire up attachSpeedChartHover — no crosshair on the PPS chart")
	}

	scs := funcSource(t, "speedComboChartSVG")
	if !strings.Contains(scs, "hoverScaffold(") {
		t.Error("speedComboChartSVG doesn't call hoverScaffold — the charts have no capture rect or hover markup for a crosshair to attach to")
	}

	cs := funcSource(t, "chartSVG")
	if !strings.Contains(cs, "hoverScaffold(") {
		t.Error("chartSVG (Metrics) no longer builds its hover markup through the shared hoverScaffold — the refactor that shared it with the speedtest charts regressed")
	}

	ash := funcSource(t, "attachSpeedChartHover")
	if strings.Contains(ash, "clockOf(") {
		t.Error("attachSpeedChartHover's tooltip uses clockOf(t) — a wall-clock read of an elapsed-seconds value, meaningless for a finished speedtest")
	}
	if !strings.Contains(ash, "snapT.toFixed(1)") {
		t.Error("attachSpeedChartHover's tooltip doesn't format the elapsed time as seconds")
	}
}

// TestChartPadLFixesLabelClipping pins the fix for a reported bug: the PPS
// chart's y-axis labels were cut off on the left ("60,873 pkts/s" rendering
// as ",873 pkts/s"). Pps values run into 5-6 digits with thousands
// separators, wider than the fixed CH.padL (76px, sized for the shorter
// Mbps/percentage-scale labels every other chart in the app uses) had room
// for, so the leading digits rendered past the left edge of the SVG and got
// clipped. speedComboChartSVG — the one chart function every speedtest
// chart uses, single-series or two — must compute its actual left padding
// from the label text itself (chartPadL), not the fixed constant.
func TestChartPadLFixesLabelClipping(t *testing.T) {
	if !strings.Contains(indexHTML, "function chartPadL(") {
		t.Fatal("chartPadL helper is missing")
	}
	body := funcSource(t, "speedComboChartSVG")
	if !strings.Contains(body, "chartPadL(") {
		t.Error("speedComboChartSVG doesn't call chartPadL — its left padding is still the fixed CH.padL, which is what clipped the PPS labels")
	}
	if strings.Contains(body, "padL=CH.padL") || strings.Contains(body, "padL = CH.padL") {
		t.Error("speedComboChartSVG still hardcodes its left padding to CH.padL directly")
	}
}

// speedtestFn returns the source of infoSpeedtest, so the assertions above are
// scoped to that function rather than matching something elsewhere in the file.
func speedtestFn(t *testing.T) string {
	t.Helper()
	return funcSource(t, "infoSpeedtest")
}
