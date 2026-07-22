//go:build (linux && (amd64 || arm64 || arm)) || freebsd || darwin || openbsd

package webadmin

// Logic shared by every Unix-like PTY backend (pty_linux.go, pty_freebsd.go,
// pty_darwin.go, pty_openbsd.go): all four represent a session the same way
// — a real PTY master as an *os.File plus the *exec.Cmd attached to its
// slave — so resizing, closing, waiting, and finding a shell to run are
// identical regardless of which OS-specific ioctls were used to allocate the
// pty in the first place. Only spawnPTY itself (and the handful of ioctl
// request codes it needs) differs per file, because that's the one part
// where Linux's /dev/ptmx+TIOCGPTN, FreeBSD's /dev/ptmx+TIOCGPTN (documented
// pts(4) — the same idea as Linux's, but its own distinct ioctl numbers),
// Darwin's /dev/ptmx+TIOCPTYGRANT/UNLK/GNAME, and OpenBSD's /dev/ptm+PTMGET
// genuinely don't share a common shape.

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"syscall"
	"unsafe"
)

// ptySupported reports whether spawnPTY can actually work on this host.
const ptySupported = true

// defaultShell returns the shell to spawn: $SHELL if set, else the invoking
// (daemon) user's shell from /etc/passwd, else /bin/sh as a last resort.
// /etc/passwd is a real, populated user database on Linux/FreeBSD/OpenBSD;
// on macOS most real accounts live in Open Directory instead and won't be
// found there, so this step is best-effort and $SHELL (or the final /bin/sh
// fallback) is what actually covers that platform in practice.
func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		// os/user's pure-Go (no cgo) fallback path doesn't always populate
		// Shell — check it, but don't depend on it being non-empty.
		if sh, err := shellForUID(u.Uid); err == nil && sh != "" {
			return sh
		}
	}
	return "/bin/sh"
}

// shellForUID reads the login shell for uid from /etc/passwd directly — a
// small, dependency-free stand-in for the libc getpwuid the "os/user" cgo
// path would otherwise use. Best-effort: any error just falls back to
// /bin/sh in defaultShell.
func shellForUID(uid string) (string, error) {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", err
	}
	for _, line := range splitLines(data) {
		fields := splitByte(line, ':')
		if len(fields) >= 7 && fields[2] == uid {
			return fields[6], nil
		}
	}
	return "", fmt.Errorf("uid %s not found", uid)
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func splitByte(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// ptySession is a running shell attached to a PTY master.
type ptySession struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// resizePTY updates the PTY's window size live (SIGWINCH is delivered to the
// foreground process group automatically by the kernel on this ioctl).
func resizePTY(s *ptySession, rows, cols int) error {
	return setWinsize(s.ptmx, rows, cols)
}

// setWinsize uses the standard library's own per-platform TIOCSWINSZ value
// (it's an exported syscall constant everywhere gravinet ships this, unlike
// TIOCGPTN and its relatives — the ones each pty_<os>.go defines by hand),
// so this one ioctl is safe to share verbatim across every Unix backend.
func setWinsize(f *os.File, rows, cols int) error {
	ws := struct{ Row, Col, Xpixel, Ypixel uint16 }{uint16(rows), uint16(cols), 0, 0}
	return ptyIoctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// ptyIoctl issues a raw ioctl(2) call. Shared for the same reason as
// setWinsize: syscall.SYS_IOCTL is itself a correct per-platform constant
// from the standard library, so only the request codes need to be
// platform-specific, not this wrapper.
func ptyIoctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// close releases the PTY master (the child, if still running, gets SIGHUP
// from the kernel once the last open master fd closes and no one else holds
// the slave open).
func (s *ptySession) close() {
	s.ptmx.Close()
}

// wait blocks until the shell exits and returns its exit code (-1 if it was
// killed by a signal rather than exiting normally).
func (s *ptySession) wait() int {
	err := s.cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
	}
	return -1
}
