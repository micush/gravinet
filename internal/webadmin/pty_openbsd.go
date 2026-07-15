//go:build openbsd

package webadmin

// PTY allocation on OpenBSD. OpenBSD doesn't have a /dev/ptmx multiplexer at
// all — instead /dev/ptm is a small control device: one PTMGET ioctl on it
// allocates a free pty pair, fixes up its ownership, and hands back *both*
// the master and slave file descriptors already open (see ptm(4)), which is
// actually simpler than the open-master/derive-slave-path dance every other
// Unix backend here has to do. Everything except this allocation step is
// shared with the other Unix backends in pty_unix_common.go.

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// openbsdPTMGET is ptm(4)'s PTMGET ioctl, defined in OpenBSD's <sys/tty.h>
// as `_IOR('t', 1, struct ptmget)`. Not exported by the standard "syscall"
// package, so computed by hand from that macro and cross-checked against
// TIOCPTSNAME, a same-shape ioctl (also `_IOR('t', n, struct ptmget)`) whose
// numeric value is independently documented — both landed on the same
// encoding, which is the confirmation this couldn't otherwise get without a
// real OpenBSD host to run it on.
const openbsdPTMGET = 0x40287401

// openbsdPtmget mirrors OpenBSD's struct ptmget (sys/tty.h) field for field:
// two ints then two 16-byte device-name buffers. cn/sn (the device path
// strings) aren't needed here since PTMGET already hands back open fds for
// both ends directly, but the fields have to stay in the struct regardless —
// removing them would shrink sizeof(ptmget) and silently desync it from the
// size baked into openbsdPTMGET's own encoding.
type openbsdPtmget struct {
	Cfd int32
	Sfd int32
	Cn  [16]byte
	Sn  [16]byte
}

// Compile-time assertion that openbsdPtmget really is 40 bytes — the size
// PTMGET's own encoding (see openbsdPTMGET's comment) assumes. If a field
// above is ever changed without updating that constant too, this line stops
// the build instead of silently sending a mismatched ioctl.
var _ [unsafe.Sizeof(openbsdPtmget{})]byte = [40]byte{}

// spawnPTY allocates a PTY, spawns shellPath (or the default shell if empty)
// attached to it as the controlling terminal, and sets the initial window
// size. See pty_linux.go's spawnPTY for the shared contract.
func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	if shellPath == "" {
		shellPath = defaultShell()
	}
	ctl, err := os.OpenFile("/dev/ptm", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptm: %w", err)
	}
	// /dev/ptm is only the control device PTMGET is issued against; the
	// master/slave fds it hands back are independent and outlive this.
	defer ctl.Close()

	var pm openbsdPtmget
	if err := ptyIoctl(ctl.Fd(), openbsdPTMGET, uintptr(unsafe.Pointer(&pm))); err != nil {
		return nil, fmt.Errorf("PTMGET: %w", err)
	}
	ptmx := os.NewFile(uintptr(pm.Cfd), "/dev/ptm-master")
	slave := os.NewFile(uintptr(pm.Sfd), "/dev/ptm-slave")
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
