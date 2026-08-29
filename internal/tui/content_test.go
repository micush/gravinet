package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func testCtx(w int) layoutCtx { return layoutCtx{pal: paletteFor("dark"), width: w} }

// linesText renders laid-out lines to plain text, which is what these tests
// assert on: a test about column alignment should not break when a colour
// changes.
func linesText(ls []line) string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = strings.TrimRight(l.text(), " ")
	}
	return strings.Join(out, "\n")
}

func TestWrap(t *testing.T) {
	got := wrap("the quick brown fox jumps", 10)
	want := []string{"the quick", "brown fox", "jumps"}
	if len(got) != len(want) {
		t.Fatalf("wrap gave %d lines: %q", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, l := range got {
		if utf8.RuneCountInString(l) > 10 {
			t.Errorf("line overflowed the width: %q", l)
		}
	}
}

func TestWrapBreaksAWordLongerThanTheLine(t *testing.T) {
	// A base64 key or a long path hits this, and overflowing would draw over
	// the card's own border.
	got := wrap("aaaaaaaaaaaaaaaaaaaa", 6)
	if len(got) != 4 {
		t.Fatalf("expected the word to be broken, got %q", got)
	}
	for _, l := range got {
		if utf8.RuneCountInString(l) > 6 {
			t.Errorf("broken piece still overflows: %q", l)
		}
	}
}

func TestWrapHonorsExplicitNewlines(t *testing.T) {
	got := wrap("one\n\ntwo", 20)
	if len(got) != 3 || got[0] != "one" || got[1] != "" || got[2] != "two" {
		t.Errorf("wrap dropped a paragraph break: %q", got)
	}
}

func TestExpandTabs(t *testing.T) {
	// mono items preserve pre-formatted output, whose columns are the point.
	// A raw tab drawn as one space misaligns exactly that.
	if got := expandTabs("a\tb"); got != "a       b" {
		t.Errorf("expandTabs = %q", got)
	}
	if got := expandTabs("no tabs"); got != "no tabs" {
		t.Errorf("expandTabs altered a tab-free string: %q", got)
	}
}

func TestLayoutCardDrawsABorderWithItsTitle(t *testing.T) {
	got := layoutCard(card{title: "networks", items: []item{
		kv{rows: []kvRow{{"name", "corp", ""}}},
	}}, testCtx(30))
	text := linesText(got)
	lines := strings.Split(text, "\n")

	if !strings.HasPrefix(lines[0], "\u250c\u2500 NETWORKS ") {
		t.Errorf("top border = %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], "\u2510") {
		t.Errorf("top border does not close: %q", lines[0])
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "\u2514") || !strings.HasSuffix(last, "\u2518") {
		t.Errorf("bottom border = %q", last)
	}
	if !strings.Contains(text, "corp") {
		t.Errorf("card lost its body:\n%s", text)
	}
}

func TestLayoutCardKeepsEveryLineInsideItsWidth(t *testing.T) {
	// Every row must be exactly the card's width, or the right-hand border
	// staircases down the screen.
	const w = 40
	got := layoutCard(card{title: "a rather long card title here", items: []item{
		para{text: strings.Repeat("word ", 40)},
		table{head: []string{"one", "two"}, rows: []tableRow{{cells: []string{"a", "b"}}}},
	}}, testCtx(w))
	for i, l := range got {
		if n := l.width(); n != w {
			t.Errorf("line %d has width %d, want %d: %q", i, n, w, l.text())
		}
	}
}

func TestLayoutKVAlignsValues(t *testing.T) {
	got := layoutKV(kv{rows: []kvRow{
		{"name", "corp", ""},
		{"subnet4", "10.42.0.0/16", ""},
		{"id", "abc", ""},
	}}, testCtx(40))
	text := strings.Split(linesText(got), "\n")
	col := strings.Index(text[0], "corp")
	for i, want := range []string{"corp", "10.42.0.0/16", "abc"} {
		if got := strings.Index(text[i], want); got != col {
			t.Errorf("row %d value starts at %d, want %d: %q", i, got, col, text[i])
		}
	}
}

func TestLayoutTableTakesWidthFromTheWidestColumn(t *testing.T) {
	// The surplus comes off the widest column first, so a long notes field
	// absorbs nearly all the loss and the short columns stay readable — which
	// is also where losing the tail costs least.
	tb := table{
		head: []string{"name", "state", "notes"},
		rows: []tableRow{
			{cells: []string{"corp", "on", strings.Repeat("x", 200)}},
			{cells: []string{"lab", "off", "short"}},
		},
	}
	got := linesText(layoutTable(tb, testCtx(40)))
	lines := strings.Split(got, "\n")
	for _, l := range lines {
		if utf8.RuneCountInString(l) > 40 {
			t.Errorf("row overflowed: %q", l)
		}
	}
	// The narrow columns must survive intact.
	if !strings.Contains(lines[2], "corp") || !strings.Contains(lines[3], "lab") {
		t.Errorf("a short column was truncated away:\n%s", got)
	}
	if !strings.Contains(lines[2], "on") || !strings.Contains(lines[3], "off") {
		t.Errorf("the state column was truncated away:\n%s", got)
	}
}

func TestLayoutTableHasARuleUnderTheHeader(t *testing.T) {
	got := linesText(layoutTable(table{
		head: []string{"a", "b"},
		rows: []tableRow{{cells: []string{"1", "2"}}},
	}, testCtx(20)))
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header, rule and one row, got %d lines:\n%s", len(lines), got)
	}
	if strings.Trim(lines[1], "\u2500") != "" {
		t.Errorf("second line should be the rule, got %q", lines[1])
	}
}

func TestLayoutMonoTruncatesRatherThanWrapping(t *testing.T) {
	// Route tables and log lines have columns that mean something; wrapping
	// them is unreadable, so they are cut and the page scrolls sideways.
	got := layoutMono(mono{lines: []string{strings.Repeat("a", 100)}}, testCtx(20))
	if len(got) != 1 {
		t.Fatalf("a long mono line was wrapped into %d lines", len(got))
	}
	if n := got[0].width(); n > 20 {
		t.Errorf("mono line width %d exceeds the pane", n)
	}
}

func TestLayoutMonoRendersEmptyExplicitly(t *testing.T) {
	got := linesText(layoutMono(mono{}, testCtx(20)))
	if got != "(empty)" {
		t.Errorf("an empty mono block should say so, got %q", got)
	}
}

func TestLayoutSeparatesCardsWithABlankLine(t *testing.T) {
	got := layout([]card{
		{title: "one", items: []item{empty{"a"}}},
		{title: "two", items: []item{empty{"b"}}},
	}, testCtx(20))
	text := strings.Split(linesText(got), "\n")
	// Three lines per card (top, body, bottom) plus one separator.
	if len(text) != 7 {
		t.Fatalf("expected 7 lines, got %d:\n%s", len(text), strings.Join(text, "\n"))
	}
	if text[3] != "" {
		t.Errorf("cards are not separated: %q", text[3])
	}
}

func TestLayoutSurvivesAbsurdlyNarrowWidths(t *testing.T) {
	// A terminal dragged to nothing must not panic or produce negative-width
	// repeats. Nobody can read the result; that is fine, it just has to hold
	// together until they drag it back.
	for _, w := range []int{1, 2, 3, 5, 8} {
		got := layout([]card{{title: "networks", items: []item{
			kv{rows: []kvRow{{"a", "b", ""}}},
			table{head: []string{"x", "y"}, rows: []tableRow{{cells: []string{"1", "2"}}}},
			para{text: "some prose here"},
		}}}, testCtx(w))
		if len(got) == 0 {
			t.Errorf("width %d produced no lines", w)
		}
	}
}
