package tui

import (
	"strings"
	"testing"
)

func TestScreenPrintClipsAtTheRightEdge(t *testing.T) {
	s := NewScreen(10, 2)
	s.Print(6, 0, "abcdefgh", style{})
	if got := s.Row(0); got != "      abcd" {
		t.Errorf("clipped row = %q", got)
	}
	// Off-screen writes are dropped rather than panicking: layout arithmetic
	// on a terminal that was just dragged narrower produces exactly these.
	s.Print(-4, 0, "x", style{})
	s.Print(0, 99, "x", style{})
	s.Set(99, 99, 'x', style{})
}

func TestPrintPadTruncatesWithAnEllipsis(t *testing.T) {
	s := NewScreen(20, 1)
	s.PrintPad(0, 0, 8, "config-history", style{})
	got := s.Row(0)
	if got != "config-\u2026" {
		t.Errorf("padded/truncated = %q", got)
	}
	// A shortened value must be visibly shortened — an address quietly losing
	// its last octet is worse than one that says it was cut.
	if !strings.HasSuffix(got, "\u2026") {
		t.Error("truncation left no ellipsis")
	}
}

func TestPrintPadFillsShortValues(t *testing.T) {
	s := NewScreen(20, 1)
	s.PrintPad(0, 0, 10, "ab", style{})
	s.Print(10, 0, "|", style{})
	if got := s.Row(0); got != "ab        |" {
		t.Errorf("padded = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	for _, c := range []struct{ in string; w int; want string }{
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abcd", 3, "ab\u2026"},
		{"abcd", 1, "\u2026"},
		{"abcd", 0, ""},
		{"\u00e9\u00e9\u00e9\u00e9", 3, "\u00e9\u00e9\u2026"}, // counted in runes, not bytes
	} {
		if got := truncate(c.in, c.w); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
}

func TestRenderFullRepaintWhenPrevIsNil(t *testing.T) {
	s := NewScreen(4, 1)
	s.Print(0, 0, "hi", style{})
	out := s.render(nil, colorMono)
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Errorf("a first frame must clear: %q", out)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("frame did not contain its text: %q", out)
	}
}

func TestRenderEmitsNothingForAnIdenticalFrame(t *testing.T) {
	a := NewScreen(10, 3)
	a.Print(0, 0, "same", style{})
	_ = a.render(nil, colorMono)

	b := NewScreen(10, 3)
	b.Print(0, 0, "same", style{})
	if out := b.render(a, colorMono); out != "" {
		t.Errorf("an unchanged frame should produce no output, got %q", out)
	}
}

func TestRenderOnlyEmitsChangedCells(t *testing.T) {
	a := NewScreen(20, 4)
	a.Print(0, 0, "unchanged line", style{})
	a.Print(0, 2, "old", style{})

	b := NewScreen(20, 4)
	b.Print(0, 0, "unchanged line", style{})
	b.Print(0, 2, "new", style{})

	out := b.render(a, colorMono)
	if strings.Contains(out, "unchanged") {
		t.Errorf("diff redrew an unchanged row: %q", out)
	}
	if !strings.Contains(out, "new") {
		t.Errorf("diff missed the changed row: %q", out)
	}
}

func TestRenderRepaintsWhenTheSizeChanges(t *testing.T) {
	// A resize makes cell-by-cell comparison compare the wrong cells, so the
	// frame must be full even though prev is non-nil.
	a := NewScreen(10, 2)
	b := NewScreen(20, 2)
	b.Print(0, 0, "x", style{})
	if out := b.render(a, colorMono); !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Errorf("a resized frame must repaint: %q", out)
	}
}

func TestCursorTo(t *testing.T) {
	// 1-based, per the standard, from 0-based screen coordinates. Off by one
	// here puts every frame one row up.
	if got := cursorTo(0, 0); got != "\x1b[1;1H" {
		t.Errorf("cursorTo(0,0) = %q", got)
	}
	if got := cursorTo(4, 9); got != "\x1b[10;5H" {
		t.Errorf("cursorTo(4,9) = %q", got)
	}
}

func TestStyleSequences(t *testing.T) {
	st := style{}.withFg(hex(0x4493f8)).withBold()
	if got := st.sequence(colorTrue); got != "\x1b[0;1;38;2;68;147;248m" {
		t.Errorf("truecolor = %q", got)
	}
	// Mono drops colour but keeps attributes, so a bold heading is still a
	// heading on a terminal that cannot colour it.
	if got := st.sequence(colorMono); got != "\x1b[0;1m" {
		t.Errorf("mono = %q", got)
	}
	if got := (style{}).sequence(colorTrue); got != "\x1b[0m" {
		t.Errorf("zero style = %q", got)
	}
}

func TestXterm256KeepsTheRailLegible(t *testing.T) {
	// The neutrals must reach the grey ramp rather than the cube, whose two
	// darkest levels (0 and 95) are both far from every one of them.
	panel := xterm256(darkPalette.panel)
	sidebar := xterm256(darkPalette.sidebar)
	lineC := xterm256(darkPalette.line)
	hover := xterm256(darkPalette.hover)
	for name, c := range map[string]uint8{"panel": panel, "sidebar": sidebar, "line": lineC, "hover": hover} {
		if c < 232 {
			t.Errorf("%s quantized to cube index %d, not the grey ramp", name, c)
		}
	}

	// panel and sidebar are allowed to collide — see xterm256's comment for
	// why that is the correct rounding, and why it is survivable. What must
	// not collide is the rail's edge or its cursor, because neither of those
	// is a fill: drawRail draws the border in --line and the cursor in
	// --hover, so both have to stay distinct from the fills behind them.
	if lineC == panel || lineC == sidebar {
		t.Errorf("line (%d) collides with a fill (panel %d, sidebar %d) — the rail loses its edge",
			lineC, panel, sidebar)
	}
	if hover == panel || hover == sidebar {
		t.Errorf("hover (%d) collides with a fill (panel %d, sidebar %d) — the rail cursor disappears",
			hover, panel, sidebar)
	}
	if lineC == hover {
		t.Errorf("line and hover both quantize to %d", lineC)
	}

	// A saturated colour must still go to the cube: the active rail tab is
	// filled with --acc, and a blue that quantized to grey would make the
	// selection invisible.
	if acc := xterm256(darkPalette.acc); acc >= 232 {
		t.Errorf("the accent blue quantized to the grey ramp (%d)", acc)
	}
	// And the two text colours have to stay apart from each other.
	if xterm256(darkPalette.fg) == xterm256(darkPalette.mut) {
		t.Error("fg and mut quantize identically — muted text stops being muted")
	}
}

func TestPaletteLeavesTheBackgroundUnset(t *testing.T) {
	// See paletteFor: painting --bg over the whole terminal produces a slab
	// that matches nobody's scheme and defeats transparency.
	for _, theme := range []string{"dark", "light"} {
		p := paletteFor(theme)
		if p.bg.set {
			t.Errorf("%s: page background should be left to the terminal", theme)
		}
		if !p.panel.set || !p.sidebar.set || !p.acc.set {
			t.Errorf("%s: the fills that make the layout legible must still be painted", theme)
		}
	}
}

func TestPaletteMatchesUIGo(t *testing.T) {
	// style.go transcribes ui.go's two :root blocks. This reads them back and
	// compares, for the same reason nav_test.go reads NAV_GROUPS: the copy is
	// fine, the copy going stale is not.
	src := uiSource(t)
	for _, c := range []struct {
		block string
		want  palette
		name  string
	}{
		{":root {", darkPalette, "dark"},
		{`:root[data-theme="light"] {`, lightPalette, "light"},
	} {
		start := strings.Index(src, c.block)
		if start < 0 {
			t.Fatalf("%s: %q not found in ui.go", c.name, c.block)
		}
		end := strings.Index(src[start:], "}")
		body := src[start : start+end]

		for _, v := range []struct {
			key  string
			have rgb
		}{
			{"--panel", c.want.panel}, {"--line", c.want.line}, {"--fg", c.want.fg},
			{"--mut", c.want.mut}, {"--acc", c.want.acc}, {"--danger", c.want.danger},
			{"--ok", c.want.ok}, {"--sidebar", c.want.sidebar}, {"--hover", c.want.hover},
			{"--warn", c.want.warn},
		} {
			i := strings.Index(body, v.key+":#")
			if i < 0 {
				t.Errorf("%s: %s not found in ui.go's block", c.name, v.key)
				continue
			}
			got := body[i+len(v.key)+2:][:6]
			want := hexString(v.have)
			if !strings.EqualFold(got, want) {
				t.Errorf("%s %s: ui.go says #%s, style.go says #%s", c.name, v.key, got, want)
			}
		}
	}
}

// hexString renders an rgb back to six hex digits, for the comparison above.
func hexString(c rgb) string {
	const d = "0123456789abcdef"
	return string([]byte{
		d[c.r>>4], d[c.r&0xf],
		d[c.g>>4], d[c.g&0xf],
		d[c.b>>4], d[c.b&0xf],
	})
}

func TestDetectColorModeIsConservative(t *testing.T) {
	// Sending 24-bit sequences to a 256-colour terminal produces visible
	// garbage; sending 256-colour ones to a truecolor terminal produces
	// slightly flatter colours nobody notices. So an unknown TERM must not
	// claim truecolor.
	t.Setenv("COLORTERM", "")
	t.Setenv("TERM", "xterm-256color")
	if got := detectColorMode(); got != color256 {
		t.Errorf("xterm-256color without COLORTERM = %v, want 256", got)
	}
	t.Setenv("COLORTERM", "truecolor")
	if got := detectColorMode(); got != colorTrue {
		t.Errorf("COLORTERM=truecolor = %v", got)
	}
	t.Setenv("COLORTERM", "")
	t.Setenv("TERM", "dumb")
	if got := detectColorMode(); got != colorMono {
		t.Errorf("TERM=dumb = %v", got)
	}
}

func TestParseColorModeOverridesDetection(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if got := parseColorMode("truecolor"); got != colorTrue {
		t.Errorf("explicit truecolor was not honored over TERM=dumb")
	}
	if got := parseColorMode(""); got != colorMono {
		t.Errorf("empty should fall through to detection")
	}
}

func TestDetectTheme(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	if got := detectTheme(); got != "dark" {
		t.Errorf("no hint should assume dark (the web admin's own default), got %q", got)
	}
	t.Setenv("COLORFGBG", "0;15")
	if got := detectTheme(); got != "light" {
		t.Errorf("COLORFGBG with a white background = %q", got)
	}
	t.Setenv("COLORFGBG", "15;default;0")
	if got := detectTheme(); got != "dark" {
		t.Errorf("three-field COLORFGBG with a dark background = %q", got)
	}
}
