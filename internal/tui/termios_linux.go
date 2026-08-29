//go:build linux

package tui

import "syscall"

// Linux's termios ioctls. Named TCGETS/TCSETS here, TIOCGETA/TIOCSETA on the
// BSDs (termios_bsd.go); the split exists only so term_unix.go can be one
// file rather than four near-identical ones.
const (
	termiosGet = syscall.TCGETS
	termiosSet = syscall.TCSETS
)
