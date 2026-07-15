//go:build darwin

package webadmin

// PTY allocation on macOS. Darwin's /dev/ptmx multiplexer doesn't report a
// simple unit number the way Linux's and FreeBSD's do — instead, three
// ioctls on the master fd do the whole job: TIOCPTYGNAME reads back the
// slave's actual device path (e.g. "/dev/ttys004"), and TIOCPTYGRANT /
// TIOCPTYUNLK are Darwin's grantpt(3)/unlockpt(3) equivalents (unlike
// FreeBSD, these are real ioctls here, not no-ops). Everything except this
// allocation step is shared with the other Unix backends in
// pty_unix_common.go.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// These ioctl request codes aren't exported by the standard "syscall"
// package. They come from Darwin's <sys/ttycom.h> and are the same on both
// architectures gravinet ships for macOS (amd64, arm64) — Darwin's ioctl
// encoding, like FreeBSD's, bakes the argument size into the request code
// rather than varying with register width.
const (
	darwinTIOCPTYGRANT = 0x20007454 // grantpt(3): fix the slave's ownership/permissions
	darwinTIOCPTYUNLK  = 0x20007452 // unlockpt(3): clear the lock on the slave
	darwinTIOCPTYGNAME = 0x40807453 // ptsname(3): read back the slave's device path (128-byte buffer)
)

// spawnPTY allocates a PTY, spawns shellPath (or the default shell if empty)
// attached to it as the controlling terminal, and sets the initial window
// size. See pty_linux.go's spawnPTY for the shared contract; the allocation
// sequence here — read the slave's name, grant, unlock, then open it — is
// the standard Darwin order (matching e.g. the widely used creack/pty
// library's own pty_darwin.go, cross-checked here for the ioctl values and
// call order since this couldn't be run on real macOS hardware to verify).
func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	if shellPath == "" {
		shellPath = defaultShell()
	}
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	slaveName, err := darwinPtsname(ptmx)
	if err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("get pty slave name: %w", err)
	}
	if err := ptyIoctl(ptmx.Fd(), darwinTIOCPTYGRANT, 0); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("grant pty: %w", err)
	}
	if err := ptyIoctl(ptmx.Fd(), darwinTIOCPTYUNLK, 0); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("open %s: %w", slaveName, err)
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

// darwinPtsname reads the slave device path for the pty master f via
// TIOCPTYGNAME. 128 bytes is TIOCPTYGNAME's own documented buffer size
// (encoded into the ioctl request code itself, alongside every other Darwin
// ioctl — see the const block above), not an arbitrary choice.
func darwinPtsname(f *os.File) (string, error) {
	var buf [128]byte
	if err := ptyIoctl(f.Fd(), darwinTIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return "", err
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i]), nil
		}
	}
	return "", errors.New("TIOCPTYGNAME: device name not NUL-terminated")
}
