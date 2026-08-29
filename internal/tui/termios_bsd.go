//go:build darwin || freebsd || openbsd

package tui

import "syscall"

// The BSD spelling of the termios ioctls, shared by Darwin, FreeBSD and
// OpenBSD. See termios_linux.go for the other half of this split.
//
// TIOCSETA rather than TIOCSETAW/TIOCSETAF: both of the latter drain (and
// TIOCSETAF also flushes) pending output first, which is the right call when
// changing line discipline on a serial port and the wrong one here — this is
// a screen, the pending output is a half-drawn frame, and flushing it is how
// a restore ends up eating the sequence that puts the cursor back.
const (
	termiosGet = syscall.TIOCGETA
	termiosSet = syscall.TIOCSETA
)
