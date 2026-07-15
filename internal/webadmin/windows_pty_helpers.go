package webadmin

// Pure helpers for the Windows ConPTY backend (pty_windows.go), split out
// into their own build-tag-free file so they can be unit tested on any
// platform rather than only on a real Windows host — none of them touch a
// Windows API directly, they just compute values that pty_windows.go's
// syscalls need.

import (
	"strings"
	"unicode/utf16"
)

// winCoord mirrors the Win32 COORD struct (X, Y as SHORTs) — named with a
// win prefix here (unlike pty_windows.go's own internal naming) simply
// because this file has no build tag and "coord" is common enough to want
// the extra clarity about what it's for.
type winCoord struct{ x, y int16 }

// pack encodes a COORD the way CreatePseudoConsole/ResizePseudoConsole
// expect it when passed through a raw uintptr-only syscall.Proc.Call rather
// than natively by value: X in the low 16 bits, Y in the high 16 bits of a
// single machine word, matching how the x86/x64 calling convention would
// have packed the two-SHORT struct into one register itself. Returns
// uint32 rather than uintptr so it's usable (and testable) without pulling
// in a platform-specific pointer width.
func (c winCoord) pack() uint32 {
	return uint32(uint16(c.y))<<16 | uint32(uint16(c.x))
}

// quoteIfNeeded wraps path in double quotes if it contains whitespace —
// CreateProcess's single command-line string needs that to treat a path
// like "C:\Program Files\..." as one token rather than several.
func quoteIfNeeded(path string) string {
	if strings.ContainsAny(path, " \t") && !strings.HasPrefix(path, `"`) {
		return `"` + path + `"`
	}
	return path
}

// buildEnvBlockUTF16 encodes envv ("KEY=VALUE" pairs) as the null-separated,
// double-null-terminated UTF-16 block CreateProcess expects for
// lpEnvironment when paired with CREATE_UNICODE_ENVIRONMENT. Mirrors the
// private helper of the same name in the standard library's own syscall
// package (exec_windows.go), which isn't exported for use outside it —
// built by hand here (rather than via syscall.UTF16PtrFromString, which
// rejects embedded NULs and so can't produce a multi-entry block at all).
// Returns the full slice (not just &slice[0]) so it stays plain, portable
// Go — pty_windows.go takes the address of element 0 itself.
func buildEnvBlockUTF16(envv []string) []uint16 {
	if len(envv) == 0 {
		return []uint16{0, 0}
	}
	var all []uint16
	for _, e := range envv {
		all = append(all, utf16.Encode([]rune(e))...)
		all = append(all, 0)
	}
	all = append(all, 0) // second, block-terminating NUL after the last entry's own
	return all
}
