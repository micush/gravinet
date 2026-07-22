//go:build freebsd

package webadmin

// PTY allocation on FreeBSD. FreeBSD 8.0 replaced the old fixed-pair BSD pty
// driver with a Unix98-style "pts" driver (see pts(4)), reached through the
// posix_openpt(2) syscall — *not* through /dev/ptmx. /dev/ptmx does exist on
// FreeBSD, but pts(4)'s own man page is explicit that it's a separate,
// legacy compatibility device ("this device should not be opened directly
// ... new devices should only be allocated with posix_openpt(2)"), provided
// by the old-style pty(4) driver for binary compat (e.g. Linux emulation) —
// and unlike pts(4) itself, that compat driver isn't loaded by default on a
// stock FreeBSD install, so open("/dev/ptmx") fails with ENOENT there ("no
// such file or directory") until something loads it. posix_openpt(2) needs
// no such module: it's pts(4)'s own native allocation path, always present
// wherever pts(4) is (i.e. every FreeBSD 8.0+). Go's stdlib "syscall"
// package exports the syscall number (SYS_POSIX_OPENPT) uniformly across
// every FreeBSD arch gravinet builds for (386/amd64/arm/arm64/riscv64 all
// use 504), so this needs no per-arch handling the way ioctl numbers
// sometimes do. Once opened, TIOCGPTN on the resulting fd works exactly the
// same either way — it's the same pts(4) master either way, just reached
// through its real syscall entry point instead of a VFS node that isn't
// guaranteed to exist. The one real difference from Linux is that there's
// no lock/unlock step — pts(4) and posix_openpt(2) hand back a slave that's
// already correctly owned and permissioned, so FreeBSD's
// grantpt(3)/unlockpt(3) are documented as pure validation with no ioctl
// behind them at all (they only check the fd is a genuine pty master).
// Everything except this allocation step is shared with the other Unix
// backends in pty_unix_common.go.

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// TIOCGPTN isn't exported by the standard "syscall" package. Its value here
// comes from FreeBSD's <sys/ttycom.h> and — unlike Linux, where a handful of
// architectures number ioctls differently — is the same across every FreeBSD
// architecture Go supports (amd64, arm64, arm, 386, riscv64, ...): FreeBSD's
// ioctl encoding bakes in the argument's byte size (4, for the "unsigned
// int" TIOCGPTN reports into) rather than varying by register width the way
// a couple of Linux ports do, so this isn't limited to a narrower build tag.
const freebsdTIOCGPTN = 0x4004740f // get the pts device number

// posixOpenpt calls posix_openpt(2) directly — see the package comment
// above for why this, not open("/dev/ptmx"), is the correct way to allocate
// a pts(4) master on FreeBSD. Go's syscall package doesn't wrap this call
// itself, only the syscall number (syscall.SYS_POSIX_OPENPT), so it's
// issued directly the same way pty_windows.go resolves ConPTY's entry
// points by hand — no golang.org/x/sys/unix needed for one syscall.
func posixOpenpt(flags int) (*os.File, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_POSIX_OPENPT, uintptr(flags), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return os.NewFile(fd, "/dev/pts/ptmx"), nil
}

// spawnPTY allocates a PTY, spawns shellPath (or the default shell if empty)
// attached to it as the controlling terminal, and sets the initial window
// size. See pty_linux.go's spawnPTY for the shared contract; this differs
// only in skipping the lock/unlock step FreeBSD doesn't have.
func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	if shellPath == "" {
		shellPath = defaultShell()
	}
	ptmx, err := posixOpenpt(os.O_RDWR | syscall.O_NOCTTY)
	if err != nil {
		return nil, fmt.Errorf("posix_openpt: %w", err)
	}
	var n int32
	if err := ptyIoctl(ptmx.Fd(), freebsdTIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
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
