package tui

// The palette, transcribed from internal/webadmin/ui.go's own :root block —
// the same hex values the browser gets, both themes:
//
//	--bg:#0f1419  --panel:#1a2129  --line:#2a3441  --fg:#e6edf3  --mut:#8b98a5
//	--acc:#4493f8 --danger:#f85149 --ok:#3fb950    --sidebar:#141b22
//	--hover:#202a35 --warn:#d29922
//
// and the light theme's replacements below them. Transcribed rather than
// parsed: ui.go is a Go string constant, not a stylesheet this package can
// read at runtime, and a build-time parse of a CSS block inside a string
// literal is more machinery than eleven colours are worth. What keeps them
// honest is style_test.go, which does read ui.go and compares — the same
// arrangement navparity_test.go has for the rail, and for the same reason:
// the copy is fine, the copy silently going stale is not.
//
// A terminal is not a browser, and two of these do not survive the trip
// unchanged. See paletteFor.

import (
	"os"
	"strings"
)

// rgb is a 24-bit colour. The zero value means "the terminal's own default",
// which is distinct from black: a screen that never sets a background lets
// the operator's transparency and their own scheme through, and on a light
// terminal being drawn dark-on-default is the difference between readable and
// not.
type rgb struct {
	r, g, b uint8
	set     bool
}

func hex(v uint32) rgb {
	return rgb{r: uint8(v >> 16), g: uint8(v >> 8), b: uint8(v), set: true}
}

// palette names the eleven colours ui.go names, using ui.go's own names so
// the two can be read side by side.
type palette struct {
	bg      rgb
	panel   rgb
	line    rgb
	fg      rgb
	mut     rgb
	acc     rgb
	danger  rgb
	ok      rgb
	sidebar rgb
	hover   rgb
	warn    rgb
}

// darkPalette and lightPalette are ui.go's two :root blocks.
var (
	darkPalette = palette{
		bg:      hex(0x0f1419),
		panel:   hex(0x1a2129),
		line:    hex(0x2a3441),
		fg:      hex(0xe6edf3),
		mut:     hex(0x8b98a5),
		acc:     hex(0x4493f8),
		danger:  hex(0xf85149),
		ok:      hex(0x3fb950),
		sidebar: hex(0x141b22),
		hover:   hex(0x202a35),
		warn:    hex(0xd29922),
	}
	lightPalette = palette{
		bg:      hex(0xf6f8fa),
		panel:   hex(0xffffff),
		line:    hex(0xd8dee4),
		fg:      hex(0x1f2328),
		mut:     hex(0x656d76),
		acc:     hex(0x0969da),
		danger:  hex(0xcf222e),
		ok:      hex(0x1a7f37),
		sidebar: hex(0xeceff2),
		hover:   hex(0xe2e7ee),
		warn:    hex(0xbf8700),
	}
)

// paletteFor returns the palette for a theme name, with the one adjustment a
// terminal needs: the page background is left unset.
//
// In a browser --bg is the whole viewport and painting it is free. In a
// terminal it is not the whole viewport — it is whatever rectangle this
// program draws, inside a window the operator has already chosen the colour
// of, often with transparency, and painting #0f1419 over all of it produces a
// near-black slab that matches nobody's scheme and defeats their
// transparency. Leaving it unset means the console sits in the terminal's own
// background, which is what every other terminal program does and what an
// operator expects. The panel, sidebar, and accent fills are still painted:
// those are the shapes that make the layout legible, and they are drawn
// against whatever the terminal's background happens to be, exactly as the
// web admin's cards are drawn against its.
func paletteFor(theme string) palette {
	p := darkPalette
	if theme == "light" {
		p = lightPalette
	}
	p.bg = rgb{}
	return p
}

// detectTheme picks a theme when none was asked for. There is no reliable way
// to ask a terminal whether it is light or dark — the OSC 11 query exists but
// is unanswered by enough emulators that waiting on it means a startup stall
// on exactly the ones that will not answer — so this reads COLORFGBG, which
// is the one widely-set hint, and otherwise assumes dark. Dark is both the
// web admin's own default and the overwhelmingly common terminal default, so
// the assumption is right far more often than not, and `-theme light` is one
// flag for the rest.
//
// COLORFGBG is "fg;bg" or "fg;default;bg" in terminal colour numbers; a
// background of 15 (white) or 7 (light grey) means a light terminal.
func detectTheme() string {
	v := os.Getenv("COLORFGBG")
	if v == "" {
		return "dark"
	}
	parts := strings.Split(v, ";")
	bg := parts[len(parts)-1]
	if bg == "15" || bg == "7" {
		return "light"
	}
	return "dark"
}

// colorMode is how much colour the terminal can be told about.
type colorMode int

const (
	colorTrue colorMode = iota // 24-bit, "\x1b[38;2;R;G;Bm"
	color256                   // the xterm 256-colour cube
	colorMono                  // no colour at all; bold/dim/reverse only
)

// detectColorMode picks a colour depth from the environment. TERM is the
// long-standing signal and COLORTERM is the one that actually distinguishes
// truecolor, since a great many terminals that support it still report
// TERM=xterm-256color.
//
// The failure modes are asymmetric, which decides the defaults. Sending
// 24-bit sequences to a terminal that only understands 256 produces visible
// garbage; sending 256-colour sequences to a truecolor terminal produces
// slightly flatter colours nobody notices. So this only claims truecolor on a
// positive signal, and treats an unknown TERM as 256-colour rather than
// assuming the better case.
func detectColorMode() colorMode {
	term := os.Getenv("TERM")
	if term == "dumb" || term == "" {
		return colorMono
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return colorTrue
	}
	if strings.Contains(term, "direct") { // e.g. xterm-direct, tmux-direct
		return colorTrue
	}
	if strings.Contains(term, "mono") {
		return colorMono
	}
	return color256
}

// parseColorMode resolves the -color flag, falling back to detection.
func parseColorMode(s string) colorMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "truecolor", "24bit", "true":
		return colorTrue
	case "256", "8bit":
		return color256
	case "mono", "none", "off":
		return colorMono
	default:
		return detectColorMode()
	}
}

// style is a foreground/background pair plus attributes. Comparable by design
// — the renderer's inner loop compares styles per cell to decide whether to
// re-emit a sequence, so this must be a plain value with no pointers in it.
type style struct {
	fg, bg    rgb
	bold      bool
	dim       bool
	underline bool
	reverse   bool
}

func (s style) withFg(c rgb) style   { s.fg = c; return s }
func (s style) withBg(c rgb) style   { s.bg = c; return s }
func (s style) withBold() style      { s.bold = true; return s }
func (s style) withDim() style       { s.dim = true; return s }
func (s style) withUnderline() style { s.underline = true; return s }
func (s style) withReverse() style   { s.reverse = true; return s }

// sequence renders this style as an SGR escape. Always starts from a reset,
// because the alternative — tracking which attributes are on and turning off
// only what changed — saves a handful of bytes per style change and gets the
// screen into a wrong state permanently the first time the accounting is off
// by one.
func (s style) sequence(cm colorMode) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	if s.bold {
		b.WriteString(";1")
	}
	if s.dim {
		b.WriteString(";2")
	}
	if s.underline {
		b.WriteString(";4")
	}
	if s.reverse {
		b.WriteString(";7")
	}
	if cm != colorMono {
		if s.fg.set {
			b.WriteString(";")
			b.WriteString(colorSeq(s.fg, cm, false))
		}
		if s.bg.set {
			b.WriteString(";")
			b.WriteString(colorSeq(s.bg, cm, true))
		}
	}
	b.WriteString("m")
	return b.String()
}

// colorSeq renders one colour as SGR parameters, without the leading
// semicolon or trailing "m".
func colorSeq(c rgb, cm colorMode, background bool) string {
	base := "38"
	if background {
		base = "48"
	}
	if cm == colorTrue {
		return base + ";2;" + itoa(int(c.r)) + ";" + itoa(int(c.g)) + ";" + itoa(int(c.b))
	}
	return base + ";5;" + itoa(int(xterm256(c)))
}

// xterm256 maps a 24-bit colour onto the xterm 256-colour palette, choosing
// between the 6x6x6 colour cube and the 24-step grey ramp by whichever is
// closer. The greys matter more than the cube here: five of the eleven
// palette entries (bg, panel, line, sidebar, hover) are near-neutral, and
// quantizing them into the cube alone would put several of them on the same
// index — the cube's darkest levels are 0 and 95, and every one of these
// sits between them.
//
// Even with the ramp, two entries do collide: --panel (#1a2129) and
// --sidebar (#141b22) are six units apart per channel and both land on grey
// 234. That is the correct rounding — the ramp's step is ten and they are
// genuinely closer to each other than to any other index — and inventing a
// difference by nudging one of them would be a lie about what the palette
// says. It is also survivable, because the rail's edge is not a fill-colour
// change: drawRail draws an explicit border character in --line, which
// quantizes to 236, and the rail cursor uses --hover at 235. So on a
// 256-colour terminal the rail keeps its edge and its highlight and loses
// only a background difference that is nearly invisible at 24 bits anyway.
// screen_test.go pins exactly that: the fills may collide, the border and
// the hover must not.
func xterm256(c rgb) uint8 {
	cubeIdx := func(v uint8) int {
		// The cube's levels are 0, 95, 135, 175, 215, 255 — not evenly
		// spaced, so this picks the nearest rather than dividing by 51.
		levels := [6]int{0, 95, 135, 175, 215, 255}
		best, bestD := 0, 1<<30
		for i, l := range levels {
			d := int(v) - l
			if d < 0 {
				d = -d
			}
			if d < bestD {
				best, bestD = i, d
			}
		}
		return best
	}
	levels := [6]int{0, 95, 135, 175, 215, 255}
	ri, gi, bi := cubeIdx(c.r), cubeIdx(c.g), cubeIdx(c.b)
	cubeDist := sq(int(c.r)-levels[ri]) + sq(int(c.g)-levels[gi]) + sq(int(c.b)-levels[bi])

	// Grey ramp: indices 232..255 are 8, 18, 28, ... 238.
	grey := (int(c.r)*299 + int(c.g)*587 + int(c.b)*114) / 1000
	gidx := (grey - 8 + 5) / 10
	if gidx < 0 {
		gidx = 0
	}
	if gidx > 23 {
		gidx = 23
	}
	gval := 8 + gidx*10
	greyDist := sq(int(c.r)-gval) + sq(int(c.g)-gval) + sq(int(c.b)-gval)

	if greyDist < cubeDist {
		return uint8(232 + gidx)
	}
	return uint8(16 + 36*ri + 6*gi + bi)
}

func sq(n int) int { return n * n }
