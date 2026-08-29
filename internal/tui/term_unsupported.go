//go:build !(linux || darwin || freebsd || openbsd || windows)

package tui

// The fallback for any GOOS gravinet does not ship a terminal backend for —
// plan9, js/wasm, solaris, and anything else. Same posture as
// internal/webadmin/pty_unsupported.go: refuse clearly rather than ship a
// half-working implementation on a target nobody has run this on.
//
// Note that this is a narrower exclusion than the PTY one. That file also
// excludes Linux architectures whose ioctl *numbering* is unverified, because
// it defines request codes by hand (TIOCGPTN and friends aren't in the
// standard library). Nothing here does: every ioctl this package issues —
// TCGETS/TCSETS, TIOCGETA/TIOCSETA, TIOCGWINSZ — is an exported syscall
// constant the standard library defines correctly per architecture, so the
// Unix backend is safe on every Linux arch Go supports, not just the three
// gravinet releases binaries for.

import (
	"errors"
	"os"
)

var errNoTerminalBackend = errors.New("gravinet tui has no terminal backend for this platform")

func isTerminal(f *os.File) bool { return false }

func enterRaw(f *os.File) (func(), error) { return nil, errNoTerminalBackend }

func termSize(f *os.File) (w, h int, ok bool) { return 0, 0, false }
