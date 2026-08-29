//go:build linux || darwin || freebsd || openbsd

package tui

// Raw mode and window size on the Unix-likes gravinet ships for, using only
// the standard library — the same posture internal/webadmin's PTY backends
// take, and for the same reason: the calls involved are three ioctls, they
// have been stable for decades, and go.mod is dependency-free.
//
// The one thing that genuinely differs across these four platforms is the
// name of the termios get/set ioctl: Linux calls them TCGETS/TCSETS, the BSDs
// (and Darwin) call them TIOCGETA/TIOCSETA. Both pairs are exported by the
// standard "syscall" package on their own platforms, so the split is two
// small files (termios_linux.go, termios_bsd.go) holding nothing but which
// pair to use — not a per-platform copy of this file.
//
// syscall.Termios' own field widths differ too (uint32 on Linux, uint64 on
// Darwin), which is exactly why the flag manipulation below is written
// against the fields and the untyped syscall constants rather than against
// any concrete integer type: it compiles unchanged on all four.

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal, by asking for its attributes
// and seeing whether the kernel objects. This is the standard test and it is
// the honest one — "can I put this in raw mode" is precisely the question
// being asked, so asking it directly beats inspecting the file mode and
// inferring.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return ioctlPtr(f.Fd(), termiosGet, unsafe.Pointer(&t)) == nil
}

// enterRaw puts f into raw mode and returns a function that restores the
// attributes it found. The returned func is safe to call more than once.
//
// "Raw" here means the cfmakeraw(3) set, with one deliberate exception noted
// at ISIG below.
func enterRaw(f *os.File) (restore func(), err error) {
	var old syscall.Termios
	if err := ioctlPtr(f.Fd(), termiosGet, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old

	// Input: no CR/NL translation (so Enter arrives as \r and stays
	// distinguishable), no XON/XOFF (Ctrl-S would otherwise freeze the
	// display with no way to say so), no parity or 8th-bit mangling of the
	// escape sequences arrow keys arrive as.
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	// Output: no post-processing, so a bare \n is not silently turned into
	// \r\n underneath the cursor positioning this draws with.
	raw.Oflag &^= syscall.OPOST
	// Local: no echo (this package draws every character it wants seen), no
	// canonical mode (keys arrive as they are pressed, not per line), no
	// IEXTEN (Ctrl-V's literal-next would swallow the following key).
	//
	// ISIG is cleared too, which is the exception worth stating: with it set,
	// Ctrl-C is delivered as SIGINT and never reaches this program as a key.
	// The event loop handles Ctrl-C itself and exits cleanly through the same
	// path 'q' uses, which is what restores the terminal — a signal-delivered
	// interrupt would exit before the deferred restore in Run could run and
	// leave the shell in raw mode with no echo. That is the single worst
	// failure this package can have, and clearing one flag closes it.
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	// Block until at least one byte, with no inter-byte timer: the event loop
	// wants a blocking read it can park on, not a spin.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := ioctlPtr(f.Fd(), termiosSet, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = ioctlPtr(f.Fd(), termiosSet, unsafe.Pointer(&old))
	}, nil
}

// termSize reports the terminal's size in character cells. A terminal that
// answers 0 for either (some serial consoles, and anything where TIOCGWINSZ
// is a no-op) is reported as not-ok so the caller can fall back rather than
// try to draw into a zero-width screen.
func termSize(f *os.File) (w, h int, ok bool) {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	if err := ioctlPtr(f.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, false
	}
	if ws.Col == 0 || ws.Row == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

// ioctlPtr issues one ioctl(2) whose argument is a pointer. Same shape and
// same reasoning as internal/webadmin's ptyIoctl: syscall.SYS_IOCTL is a
// correct per-platform constant already, so only the request code needs to
// vary, not the wrapper.
//
// The unsafe.Pointer argument is taken (rather than a uintptr) so that the
// conversion to uintptr happens in the Syscall call expression itself, which
// is the one form the unsafe.Pointer rules guarantee is safe against the
// garbage collector moving the pointee.
func ioctlPtr(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
