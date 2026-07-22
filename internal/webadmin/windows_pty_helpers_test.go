package webadmin

import "testing"

// TestWinCoordPack verifies winCoord.pack() puts X in the low 16 bits and Y
// in the high 16 bits of the returned word — the layout
// CreatePseudoConsole/ResizePseudoConsole expect when a COORD is passed
// through a raw uintptr-only syscall rather than natively by value.
func TestWinCoordPack(t *testing.T) {
	cases := []struct {
		c    winCoord
		want uint32
	}{
		{winCoord{x: 80, y: 24}, 24<<16 | 80},
		{winCoord{x: 0, y: 0}, 0},
		{winCoord{x: 1, y: 0}, 1},
		{winCoord{x: 0, y: 1}, 1 << 16},
		{winCoord{x: 255, y: 255}, 255<<16 | 255},
		// Terminal dimensions are always small positive numbers in practice,
		// but the encoding should still round-trip cleanly at int16's max.
		{winCoord{x: 32767, y: 32767}, 32767<<16 | 32767},
	}
	for _, c := range cases {
		if got := c.c.pack(); got != c.want {
			t.Errorf("winCoord{%d,%d}.pack() = 0x%08x, want 0x%08x", c.c.x, c.c.y, got, c.want)
		}
	}
}

func TestQuoteIfNeeded(t *testing.T) {
	cases := []struct{ in, want string }{
		{`cmd.exe`, `cmd.exe`},
		{`C:\Windows\System32\cmd.exe`, `C:\Windows\System32\cmd.exe`},
		{`C:\Program Files\Git\bin\bash.exe`, `"C:\Program Files\Git\bin\bash.exe"`},
		{`"C:\already quoted\x.exe"`, `"C:\already quoted\x.exe"`}, // already quoted: don't double-wrap
		{"has\ttab", "\"has\ttab\""},
	}
	for _, c := range cases {
		if got := quoteIfNeeded(c.in); got != c.want {
			t.Errorf("quoteIfNeeded(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildEnvBlockUTF16 checks the null-separated, double-null-terminated
// shape CreateProcess requires for lpEnvironment under
// CREATE_UNICODE_ENVIRONMENT: each entry followed by one NUL, and one more
// trailing NUL after the last entry's own — i.e. two consecutive zero
// uint16s at the very end, and nowhere else.
func TestBuildEnvBlockUTF16(t *testing.T) {
	block := buildEnvBlockUTF16([]string{"A=1", "B=2"})
	want := []uint16{'A', '=', '1', 0, 'B', '=', '2', 0, 0}
	if len(block) != len(want) {
		t.Fatalf("len = %d, want %d (block=%v)", len(block), len(want), block)
	}
	for i := range want {
		if block[i] != want[i] {
			t.Fatalf("block[%d] = %d, want %d (block=%v)", i, block[i], want[i], block)
		}
	}

	// Empty input must still produce the minimal valid double-NUL block —
	// CreateProcess treats a single trailing NUL as an unterminated,
	// truncated block, not an empty one.
	empty := buildEnvBlockUTF16(nil)
	if len(empty) != 2 || empty[0] != 0 || empty[1] != 0 {
		t.Fatalf("buildEnvBlockUTF16(nil) = %v, want [0 0]", empty)
	}

	// A single entry should still get both its own separator and the
	// block-terminating NUL — i.e. two trailing zeros, not one.
	single := buildEnvBlockUTF16([]string{"X=1"})
	wantSingle := []uint16{'X', '=', '1', 0, 0}
	if len(single) != len(wantSingle) {
		t.Fatalf("single-entry len = %d, want %d (block=%v)", len(single), len(wantSingle), single)
	}
	for i := range wantSingle {
		if single[i] != wantSingle[i] {
			t.Fatalf("single[%d] = %d, want %d (block=%v)", i, single[i], wantSingle[i], single)
		}
	}
}
