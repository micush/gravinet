//go:build linux && (amd64 || arm64 || arm)

package webadmin

// PTY allocation on Linux, using only the standard library. Go's stdlib
// doesn't expose a friendly "open a pty" call (that's what packages like
// golang.org/x/sys/unix or github.com/creack/pty are usually for), but
// gravinet has zero third-party dependencies and this is exactly the kind
// of thing worth keeping that way for: the actual syscalls involved are
// small, stable, and specific to /dev/ptmx's Linux multiplexing device.
// Everything except this allocation step (defaultShell, resizePTY, close,
// wait, ...) is shared with the other Unix backends in pty_unix_common.go.

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// TIOCGPTN and TIOCSPTLCK are not exported by the standard "syscall"
// package (unlike e.g. TIOCSWINSZ, which is), so their ioctl request codes
// are given here directly. They come from Linux's <asm-generic/ioctls.h> and
// are the same on every architecture Go supports that uses the generic ioctl
// numbering — which includes amd64, arm64, and arm (this file's build tag).
// A handful of Linux architectures gravinet doesn't ship for (sparc, mips,
// powerpc, and a few others) number ioctls differently and fall through to
// pty_unsupported.go instead, rather than risk sending the wrong request
// code on a target this hasn't been verified against.
const (
	linuxTIOCGPTN   = 0x80045430 // get the pts device number
	linuxTIOCSPTLCK = 0x40045431 // lock/unlock the pts device (0 = unlock)
)

// spawnPTY allocates a PTY, spawns shellPath (or the default shell if empty)
// attached to it as the controlling terminal, and sets the initial window
// size. The caller reads/writes ptySession.ptmx for the session's I/O and
// should call resizePTY on a resize event and cmd.Wait() (or poll cmd's
// ProcessState) to learn the exit code.
func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	if shellPath == "" {
		shellPath = defaultShell()
	}
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock the slave (grantpt equivalent — Linux's /dev/ptmx starts locked).
	var unlock int32
	if err := ptyIoctl(ptmx.Fd(), linuxTIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	var n int32
	if err := ptyIoctl(ptmx.Fd(), linuxTIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("get pty number: %w", err)
	}
	ptsName := fmt.Sprintf("/dev/pts/%d", n)
	slave, err := os.OpenFile(ptsName, os.O_RDWR, 0)
	if err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("open %s: %w", ptsName, err)
	}
	defer slave.Close() // the child inherits its own copy; this one is only needed to spawn it

	if err := setWinsize(ptmx, rows, cols); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("set initial window size: %w", err)
	}

	cmd := exec.Command(shellPath)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true, // detach from the daemon's own session
		Setctty: true, // and adopt the slave (fd 0 below) as controlling tty
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("start %s: %w", shellPath, err)
	}
	return &ptySession{ptmx: ptmx, cmd: cmd}, nil
}
