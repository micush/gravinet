//go:build !((linux && (amd64 || arm64 || arm)) || freebsd || darwin || openbsd || windows)

package webadmin

// Real PTY support is now implemented for every platform gravinet actually
// ships (see build-release.sh's TARGETS): pty_linux.go, pty_freebsd.go,
// pty_darwin.go, pty_openbsd.go, and pty_windows.go. This file is the
// fallback for everything else — Linux architectures gravinet doesn't
// release for (whose ioctl numbering hasn't been verified — see
// pty_linux.go's own comment on that), and any other GOOS entirely (plan9,
// js/wasm, solaris, ...). Rather than ship a half-working or silently-wrong
// implementation on those, AllowRemoteShell simply refuses to start a
// session there with a clear error, on every OS/arch this file's build tag
// matches.

import "os"

const ptySupported = false

// ptySession's shape must match every real implementation's closely enough
// for this to compile: shell.go's pump functions (shared, platform-agnostic
// code) reference sess.ptmx directly rather than through an interface, so
// every build tag variant needs a compatible field even though spawnPTY
// here never actually populates one — it always errors first. *os.File
// satisfies this the same way it does for every Unix backend (see
// pty_unix_common.go); Windows' pipeIO also implements Read/Write/Close, so
// either shape would do here — *os.File is simplest for a value that's
// never actually used.
type ptySession struct {
	ptmx *os.File
}

func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	return nil, errShellUnsupported
}

func resizePTY(s *ptySession, rows, cols int) error {
	return errShellUnsupported
}

func (s *ptySession) close() {}

func (s *ptySession) wait() int { return -1 }
