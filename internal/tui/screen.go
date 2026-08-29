package tui

// The drawing surface: a grid of styled cells, plus a writer that turns two
// consecutive grids into the shortest run of escape sequences that gets from
// one to the other.
//
// Diffing rather than redrawing matters more here than it would in a game
// loop. This is frequently going to be running over ssh on a link that is
// itself the thing being debugged, and a full 200x60 repaint is around
// 40 KB of escape sequences three times a second. The frames this draws
// mostly differ in a handful of cells — a clock, a byte counter, one
// highlighted row — so the diff is usually a few dozen bytes.

import (
	"os"
	"strings"
	"unicode/utf8"
)

// cell is one character position: what is in it and how it is painted.
type cell struct {
	r rune
	s style
}

// Screen is an addressable grid of cells. It has no connection to a terminal
// at all — that is the whole point. The model draws into one of these and the
// tests read it back with String; only render() ever produces bytes.
type Screen struct {
	w, h  int
	cells []cell
}

// NewScreen returns a blank screen of the given size, filled with spaces in
// the default style. Sizes below 1 are clamped rather than rejected: a
// terminal that reports a nonsense size should produce a blank screen, not a
// panic in the middle of a resize.
func NewScreen(w, h int) *Screen {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	s := &Screen{w: w, h: h, cells: make([]cell, w*h)}
	s.Fill(0, 0, w, h, ' ', style{})
	return s
}

// Size reports the screen's dimensions.
func (s *Screen) Size() (w, h int) { return s.w, s.h }

// Fill paints a rectangle with one rune and style. Clipped to the screen, so
// a caller may pass a rectangle that runs off the edge without checking
// first — which every caller here does, because layout arithmetic on a
// terminal that just got dragged narrower produces exactly that.
func (s *Screen) Fill(x, y, w, h int, r rune, st style) {
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			s.Set(col, row, r, st)
		}
	}
}

// Set writes one cell. Out-of-range coordinates are dropped silently, for the
// reason given on Fill.
func (s *Screen) Set(x, y int, r rune, st style) {
	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		return
	}
	s.cells[y*s.w+x] = cell{r: r, s: st}
}

// Print writes a string starting at (x, y) and returns the column just past
// the last cell written. Clipped at the right edge; newlines are not honored
// (a caller that wants a second line asks for one, because a string that
// wraps silently is how a card ends up drawn over its own border).
func (s *Screen) Print(x, y int, text string, st style) int {
	col := x
	for _, r := range text {
		if col >= s.w {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		s.Set(col, y, r, st)
		col++
	}
	return col
}

// PrintPad writes text into a field exactly w cells wide, padding with spaces
// and truncating with a single-character ellipsis when it does not fit. Every
// table cell and rail entry goes through this, so column alignment is one
// implementation rather than one per call site.
func (s *Screen) PrintPad(x, y, w int, text string, st style) {
	if w <= 0 {
		return
	}
	t := truncate(text, w)
	col := s.Print(x, y, t, st)
	for ; col < x+w; col++ {
		s.Set(col, y, ' ', st)
	}
}

// String renders the screen as plain text, one line per row, trailing spaces
// trimmed. This is what the tests assert against: it drops all styling, which
// keeps a test about layout from breaking when a color changes.
func (s *Screen) String() string {
	var b strings.Builder
	for y := 0; y < s.h; y++ {
		line := make([]rune, 0, s.w)
		for x := 0; x < s.w; x++ {
			line = append(line, s.cells[y*s.w+x].r)
		}
		b.WriteString(strings.TrimRight(string(line), " "))
		if y < s.h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Row returns one row as plain text, trailing spaces trimmed. A convenience
// for tests that care about a single line (the top bar, a footer) and would
// otherwise split String's output.
func (s *Screen) Row(y int) string {
	if y < 0 || y >= s.h {
		return ""
	}
	line := make([]rune, 0, s.w)
	for x := 0; x < s.w; x++ {
		line = append(line, s.cells[y*s.w+x].r)
	}
	return strings.TrimRight(string(line), " ")
}

// StyleAt reports the style of one cell, for the handful of tests that are
// specifically about painting (the active rail tab, a danger-colored value)
// rather than about layout.
func (s *Screen) StyleAt(x, y int) style {
	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		return style{}
	}
	return s.cells[y*s.w+x].s
}

// render writes the sequences that turn prev into s. A nil prev (or one of a
// different size, which is what a resize produces) forces a full repaint,
// since cell-by-cell comparison against a differently-shaped grid compares
// the wrong cells.
//
// The cursor is left hidden by the caller for the duration of a session, so
// this does not move it back anywhere in particular when it finishes.
func (s *Screen) render(prev *Screen, cm colorMode) string {
	var b strings.Builder
	full := prev == nil || prev.w != s.w || prev.h != s.h
	if full {
		b.WriteString("\x1b[H\x1b[2J")
	}

	var cur style
	var curValid bool
	lastX, lastY := -1, -1

	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			c := s.cells[y*s.w+x]
			if !full {
				p := prev.cells[y*prev.w+x]
				if p == c {
					continue
				}
			}
			// Only emit a cursor move when the run is not already contiguous
			// with the last cell written. On a frame where one row changed
			// this saves a move per cell; on a full repaint it saves one per
			// cell but the first of each row.
			if y != lastY || x != lastX+1 {
				b.WriteString(cursorTo(x, y))
			}
			if !curValid || c.s != cur {
				b.WriteString(c.s.sequence(cm))
				cur, curValid = c.s, true
			}
			r := c.r
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
			lastX, lastY = x, y
		}
	}
	if curValid {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// cursorTo is the absolute cursor-position sequence. 1-based, per the
// standard, from 0-based screen coordinates.
func cursorTo(x, y int) string {
	return "\x1b[" + itoa(y+1) + ";" + itoa(x+1) + "H"
}

// itoa is strconv.Itoa without the import, used only in the render hot path
// where it is called a few times per changed cell.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// truncate cuts s to at most w display cells, marking a cut with a trailing
// ellipsis so a shortened value never reads as a complete one — an address
// silently losing its last octet is worse than one that visibly did.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "\u2026"
	}
	out := make([]rune, 0, w)
	for _, r := range s {
		if len(out) == w-1 {
			break
		}
		out = append(out, r)
	}
	return string(out) + "\u2026"
}

// ---- terminal-level sequences -------------------------------------------
//
// These four are plain ANSI/xterm and work identically on every backend
// (Windows included, once term_windows.go has turned on VT processing), so
// they live here rather than behind the per-platform split.

// enterAltScreen switches to the alternate screen buffer and hides the
// cursor. The alternate buffer is what makes quitting restore the operator's
// scrollback intact rather than leaving a screenful of dead UI in it.
func enterAltScreen(out *os.File) error {
	_, err := out.WriteString("\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J")
	return err
}

// leaveAltScreen puts the cursor back, restores the primary buffer, and
// clears any style still in effect. Best-effort: this runs on the way out,
// including out of a failure, and there is nothing useful to do with an error
// writing to a terminal that is already going away.
func leaveAltScreen(out *os.File) {
	_, _ = out.WriteString("\x1b[0m\x1b[?25h\x1b[?1049l")
}
