//go:build windows

package tui

// Raw mode and window size on Windows. The console API is not termios and
// does not pretend to be: instead of clearing flags on a struct, "raw" here
// means clearing three input modes (line input, echo, and the Ctrl-C
// processing that would otherwise never let this program see the keystroke)
// and setting two more — one on input so keys arrive as the VT escape
// sequences keys.go already decodes, one on output so the cursor positioning
// and color sequences this package writes are interpreted rather than printed
// literally.
//
// Both of those VT modes are Windows 10 1809+ / Server 2019+, which is what
// gravinet's own installer targets (install/install-windows.ps1) and the same
// floor internal/webadmin's ConPTY backend already assumes. Older consoles
// fail the SetConsoleMode call, which is reported rather than worked around:
// there is no sensible way to draw this with the pre-VT console API, and a
// clear error naming the requirement beats a screen of escape sequences.
//
// syscall exports GetConsoleMode and SetConsoleMode but not
// GetConsoleScreenBufferInfo, so that one is resolved from kernel32 directly
// via syscall.NewLazyDLL — the same zero-dependency approach pty_windows.go
// takes for the ConPTY entry points.

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	modKernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenBufferInfo = modKernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	// Input modes, from <consoleapi.h>.
	enableProcessedInput      = 0x0001 // Ctrl-C becomes a signal instead of a key
	enableLineInput           = 0x0002 // reads block until Enter
	enableEchoInput           = 0x0004 // the console draws what is typed
	enableVirtualTerminalIn   = 0x0200 // keys arrive as VT sequences
	enableWindowInput         = 0x0008 // resize events land in the input queue
	// Output modes.
	enableVirtualTerminalOut  = 0x0004 // VT sequences are interpreted on write
	disableNewlineAutoReturn  = 0x0008 // a bare \n does not also carry a \r
)

// coord/smallRect/consoleScreenBufferInfo mirror the CONSOLE_SCREEN_BUFFER_INFO
// layout. Only the window rectangle is read; the rest is present so the
// struct is the size the API expects to fill.
type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// isTerminal reports whether f is a console handle, by asking for its mode.
// A redirected stdout is a file handle and fails this, which is exactly the
// case Run wants to refuse.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

// enterRaw switches the console to the modes this package draws under and
// returns a function restoring what it found. Both the input handle and the
// output handle are touched, so both are restored — output's VT mode is
// usually already on in Windows Terminal but is not on conhost, and leaving
// it on for a shell that did not ask for it is not this program's decision to
// make.
func enterRaw(f *os.File) (restore func(), err error) {
	inH := syscall.Handle(f.Fd())
	outH := syscall.Handle(os.Stdout.Fd())

	var oldIn, oldOut uint32
	if err := syscall.GetConsoleMode(inH, &oldIn); err != nil {
		return nil, err
	}
	if err := syscall.GetConsoleMode(outH, &oldOut); err != nil {
		return nil, err
	}

	newIn := oldIn
	newIn &^= enableProcessedInput | enableLineInput | enableEchoInput
	newIn |= enableVirtualTerminalIn | enableWindowInput
	newOut := oldOut | enableVirtualTerminalOut | disableNewlineAutoReturn

	if err := setConsoleMode(inH, newIn); err != nil {
		return nil, fmt.Errorf("this console does not support virtual terminal input"+
			" (Windows 10 1809 / Server 2019 or newer is required): %w", err)
	}
	if err := setConsoleMode(outH, newOut); err != nil {
		_ = setConsoleMode(inH, oldIn)
		return nil, fmt.Errorf("this console does not support virtual terminal output"+
			" (Windows 10 1809 / Server 2019 or newer is required): %w", err)
	}

	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = setConsoleMode(inH, oldIn)
		_ = setConsoleMode(outH, oldOut)
	}, nil
}

// setConsoleMode wraps syscall.SetConsoleMode so both call sites read the
// same and the error is a plain error rather than a bool.
func setConsoleMode(h syscall.Handle, mode uint32) error {
	return syscall.SetConsoleMode(h, mode)
}

// termSize reports the console window's size in cells — the *window*
// rectangle, not the screen buffer's Size, which on conhost is the
// scrollback height (often 9001 lines) and would have this drawing a frame
// far taller than anything visible.
func termSize(f *os.File) (w, h int, ok bool) {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0, false
	}
	w = int(info.Window.Right-info.Window.Left) + 1
	h = int(info.Window.Bottom-info.Window.Top) + 1
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
