//go:build windows

package webadmin

// PTY allocation on Windows, via the ConPTY API (Windows 10 1809+ / Windows
// Server 2019+ — see ptySupported below for what happens on anything
// older). Windows has no pty concept in the Unix sense at all: instead of a
// single master fd, ConPTY gives back two one-directional pipes (one the
// child's console input comes from, one its output goes to) and a console
// host process behind the scenes that translates between them and the VT
// sequences a real terminal (xterm.js, here) expects. Built entirely on the
// standard library's "syscall" package — CreatePseudoConsole and its four
// companion calls aren't wrapped there, so those five are resolved directly
// from kernel32.dll via syscall.NewLazyDLL, the same zero-dependency
// approach pty_linux.go takes for Linux's PTY ioctls. The call sequence and
// struct layouts below are cross-checked against the well-known
// github.com/UserExistsError/conpty (itself a thin, widely used wrapper
// around Microsoft's own documented sample) rather than derived from
// scratch, since this couldn't be run on a real Windows host to verify.
// winCoord/quoteIfNeeded/buildEnvBlockUTF16 live in windows_pty_helpers.go,
// split out so that part is unit-tested on every platform, not just this one.

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modKernel32                           = syscall.NewLazyDLL("kernel32.dll")
	procCreatePseudoConsole               = modKernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole               = modKernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole                = modKernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = modKernel32.NewProc("UpdateProcThreadAttribute")
)

const (
	procThreadAttributePseudoconsole = 0x00020016
	extendedStartupInfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
)

// ptySupported is true unconditionally: gravinet's installer targets Windows
// 10/Server 2019 and newer (see install/install-windows.ps1), which all
// have ConPTY. spawnPTY still re-checks at call time (findConPtyProcs
// below) rather than trusting that blindly, so a shell session fails with a
// clear error instead of crashing the daemon if it's ever run somewhere
// older — syscall.LazyProc.Call panics on a DLL export that doesn't exist,
// so that check has to happen before any of these are ever Call()ed, not
// just once at startup.
const ptySupported = true

// findConPtyProcs resolves every ConPTY entry point up front and returns a
// clear error naming what's missing instead of leaving spawnPTY to panic
// through LazyProc.Call the first time it reaches a Windows version without
// ConPTY.
func findConPtyProcs() error {
	for _, p := range []*syscall.LazyProc{
		procCreatePseudoConsole, procResizePseudoConsole, procClosePseudoConsole,
		procInitializeProcThreadAttributeList, procUpdateProcThreadAttribute,
	} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("ConPTY (%s) not available on this Windows version — remote shell needs Windows 10 1809+ or Windows Server 2019+: %w", p.Name, err)
		}
	}
	return nil
}

// defaultShell returns the shell to spawn: $SHELL if set (rare on Windows,
// but honored the same way the Unix backends do — e.g. inside a POSIX-ish
// environment that sets it), else %COMSPEC% (normally cmd.exe), else a bare
// "cmd.exe" as a last resort.
func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if sh := os.Getenv("COMSPEC"); sh != "" {
		return sh
	}
	return "cmd.exe"
}

// pipeIO adapts ConPTY's two one-directional pipes (the end this process
// reads the shell's output from, the end it writes keystrokes into) to the
// single io.ReadWriteCloser shell.go's pump loops expect from
// ptySession.ptmx.
type pipeIO struct {
	r, w syscall.Handle
}

func (p *pipeIO) Read(b []byte) (int, error) {
	var n uint32
	err := syscall.ReadFile(p.r, b, &n, nil)
	return int(n), err
}

func (p *pipeIO) Write(b []byte) (int, error) {
	var n uint32
	err := syscall.WriteFile(p.w, b, &n, nil)
	return int(n), err
}

func (p *pipeIO) Close() error {
	e1 := syscall.CloseHandle(p.r)
	e2 := syscall.CloseHandle(p.w)
	if e1 != nil {
		return e1
	}
	return e2
}

// ptySession is a running shell attached to a Windows pseudo console. Unlike
// every Unix backend's *os.File master, ptmx here is the pipeIO pair
// wrapping the two pipe ends this process owns; see the package comment
// above for why ConPTY needs two pipes instead of one fd.
type ptySession struct {
	ptmx *pipeIO

	hpc                   uintptr        // HPCON
	consoleIn, consoleOut syscall.Handle // the pipe ends handed to ConPTY itself
	attrListBuf           []byte         // backing storage for the attribute list, kept alive until close()
	process, thread       syscall.Handle

	// closeOnce guards close() below. shell.go's pumpShellSession runs a
	// pty->browser goroutine and a browser->pty goroutine against the same
	// *ptySession, and either one can call close() independently the moment
	// its side of the connection errors — so on an ordinary disconnect both
	// can race into close() at once. That's harmless on the Unix backends
	// (pty_unix_common.go's close() just calls (*os.File).Close(), which Go
	// already reference-counts against a concurrent double-close), but every
	// call below is a raw, unguarded Win32 CloseHandle/ClosePseudoConsole —
	// closing the same handle value twice is not a benign no-op the way
	// POSIX close(2) on an already-closed fd is. If anything else in this
	// process (a new peer connection, a file, another pipe) gets allocated a
	// handle in the gap between the two closes, it can be handed that exact
	// numeric value, and the second, stale CloseHandle call then closes that
	// unrelated, currently-in-use handle instead — in a service that's
	// constantly opening/closing handles for mesh peers, that's a real
	// handle-recycling race, not a theoretical one, and it can take down
	// something the process still needs. closeOnce makes close() idempotent
	// so the real teardown only ever runs once, no matter how many
	// goroutines call it or how many times.
	closeOnce sync.Once
}

// spawnPTY allocates a ConPTY, spawns shellPath (or the default shell if
// empty) attached to it, and sets the initial size. See pty_linux.go's
// spawnPTY for the shared cross-platform contract this fulfills.
func spawnPTY(shellPath string, rows, cols int) (*ptySession, error) {
	if err := findConPtyProcs(); err != nil {
		return nil, err
	}
	if shellPath == "" {
		shellPath = defaultShell()
	}

	// Two pipes: one carries input from us into the console (we hold the
	// write end, ConPTY reads the other), one carries output from the
	// console back to us (ConPTY writes into it, we read the other end).
	var consoleIn, cmdIn, cmdOut, consoleOut syscall.Handle
	if err := syscall.CreatePipe(&consoleIn, &cmdIn, nil, 0); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	if err := syscall.CreatePipe(&cmdOut, &consoleOut, nil, 0); err != nil {
		syscall.CloseHandle(consoleIn)
		syscall.CloseHandle(cmdIn)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	hpc, err := createPseudoConsole(winCoord{int16(cols), int16(rows)}, consoleIn, consoleOut)
	if err != nil {
		syscall.CloseHandle(consoleIn)
		syscall.CloseHandle(consoleOut)
		syscall.CloseHandle(cmdIn)
		syscall.CloseHandle(cmdOut)
		return nil, err
	}

	process, thread, attrBuf, err := startProcessAttachedToConsole(hpc, shellPath)
	if err != nil {
		closePseudoConsole(hpc)
		syscall.CloseHandle(consoleIn)
		syscall.CloseHandle(consoleOut)
		syscall.CloseHandle(cmdIn)
		syscall.CloseHandle(cmdOut)
		return nil, err
	}

	return &ptySession{
		ptmx:        &pipeIO{r: cmdOut, w: cmdIn},
		hpc:         hpc,
		consoleIn:   consoleIn,
		consoleOut:  consoleOut,
		attrListBuf: attrBuf,
		process:     process,
		thread:      thread,
	}, nil
}

func createPseudoConsole(size winCoord, hIn, hOut syscall.Handle) (uintptr, error) {
	var hpc uintptr
	ret, _, callErr := procCreatePseudoConsole.Call(uintptr(size.pack()), uintptr(hIn), uintptr(hOut), 0, uintptr(unsafe.Pointer(&hpc)))
	if ret != 0 { // S_OK == 0; any other HRESULT is a failure
		return 0, fmt.Errorf("CreatePseudoConsole failed (hresult 0x%x): %w", ret, callErr)
	}
	return hpc, nil
}

func closePseudoConsole(hpc uintptr) {
	procClosePseudoConsole.Call(hpc) // void return
}

// resizePTY updates the pseudo console's window size live.
func resizePTY(s *ptySession, rows, cols int) error {
	ret, _, callErr := procResizePseudoConsole.Call(s.hpc, uintptr(winCoord{int16(cols), int16(rows)}.pack()))
	if ret != 0 {
		return fmt.Errorf("ResizePseudoConsole failed (hresult 0x%x): %w", ret, callErr)
	}
	return nil
}

// startupInfoEx mirrors STARTUPINFOEXW's layout: a full StartupInfo followed
// by one pointer-sized field for the attribute list. syscall.StartupInfo
// already matches STARTUPINFOW field for field, and (ending as it does in
// three Handle fields) its own size is already pointer-aligned, so no
// padding needs to be inserted between the two for this to line up with the
// real C struct the OS reads.
type startupInfoEx struct {
	startupInfo syscall.StartupInfo
	attrList    uintptr
}

// Compile-time assertion that no padding snuck in between the two fields:
// startupInfoEx must be exactly sizeof(StartupInfo)+sizeof(uintptr) for
// si.startupInfo.Cb (set from unsafe.Sizeof(si) in
// startProcessAttachedToConsole) to match the real STARTUPINFOEXW size the
// OS expects, and for attrList to actually sit where the OS looks for
// lpAttributeList.
var _ [unsafe.Sizeof(startupInfoEx{})]byte = [unsafe.Sizeof(syscall.StartupInfo{}) + unsafe.Sizeof(uintptr(0))]byte{}

// startProcessAttachedToConsole builds the extended startup info that binds
// hpc to the new process — via the PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE
// attribute, the mechanism ConPTY uses in place of ordinary handle
// inheritance — and starts shellPath under it. attrBuf backs the attribute
// list and must be kept alive for as long as the process might still
// reference it, hence it's returned (into ptySession.attrListBuf) rather
// than freed here.
func startProcessAttachedToConsole(hpc uintptr, shellPath string) (process, thread syscall.Handle, attrBuf []byte, err error) {
	var size uintptr
	// First call deliberately fails (empty buffer) — its only purpose is to
	// report the buffer size this needs via size, the standard Win32
	// "call once to discover the size" pattern.
	procInitializeProcThreadAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return 0, 0, nil, fmt.Errorf("InitializeProcThreadAttributeList: could not determine buffer size")
	}
	attrBuf = make([]byte, size)
	ret, _, callErr := procInitializeProcThreadAttributeList.Call(
		uintptr(unsafe.Pointer(&attrBuf[0])), 1, 0, uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return 0, 0, nil, fmt.Errorf("InitializeProcThreadAttributeList: %w", callErr)
	}
	// hpc is passed by value here (not a pointer to it) — Microsoft's own
	// ConPTY sample does the same for this specific attribute: the HPCON's
	// own bit pattern is used directly as lpValue, with cbSize=sizeof(HPCON),
	// rather than the pointer-to-a-variable most other attributes expect.
	ret, _, callErr = procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&attrBuf[0])), 0, procThreadAttributePseudoconsole,
		hpc, unsafe.Sizeof(hpc), 0, 0)
	if ret == 0 {
		return 0, 0, nil, fmt.Errorf("UpdateProcThreadAttribute: %w", callErr)
	}

	var si startupInfoEx
	si.startupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.startupInfo.Flags |= syscall.STARTF_USESTDHANDLES
	si.attrList = uintptr(unsafe.Pointer(&attrBuf[0]))

	cmdLine, err := syscall.UTF16PtrFromString(quoteIfNeeded(shellPath))
	if err != nil {
		return 0, 0, nil, err
	}
	envBlock := buildEnvBlockUTF16(append(os.Environ(), "TERM=xterm-256color"))

	var pi syscall.ProcessInformation
	flags := uint32(extendedStartupInfoPresent | createUnicodeEnvironment)
	if err := syscall.CreateProcess(nil, cmdLine, nil, nil, false, flags, &envBlock[0], nil, &si.startupInfo, &pi); err != nil {
		return 0, 0, nil, fmt.Errorf("CreateProcess: %w", err)
	}
	return pi.Process, pi.Thread, attrBuf, nil
}

// close tears the session down in the order ConPTY needs to avoid a known
// deadlock hazard: Microsoft's own docs warn that calling ClosePseudoConsole
// while still reading the output pipe can hang on Windows versions before
// 11 24H2, and a real lingering-conhost bug (microsoft/terminal#4050) means
// a pending read on that pipe isn't reliably unblocked by the child's death
// either — so ptmx (which owns that pipe) is closed first, unconditionally,
// before ClosePseudoConsole is ever called.
//
// s.process/s.thread are deliberately left open here rather than closed:
// wait() (running concurrently in its own goroutine, per shell.go's pump
// loops) may still have an outstanding WaitForSingleObject on s.process, and
// closing a handle out from under a concurrent wait on it is a documented
// Windows hazard. ClosePseudoConsole already guarantees the attached
// process (and, for a shell, everything in its process tree) is terminated,
// so wait() unblocks on its own shortly after this returns and closes both
// handles itself once it does.
func (s *ptySession) close() {
	s.closeOnce.Do(func() {
		s.ptmx.Close()
		closePseudoConsole(s.hpc)
		syscall.CloseHandle(s.consoleIn)
		syscall.CloseHandle(s.consoleOut)
	})
}

// wait blocks until the shell exits and returns its exit code, then closes
// the process/thread handles close() deliberately left open (see its own
// comment for why).
func (s *ptySession) wait() int {
	syscall.WaitForSingleObject(s.process, syscall.INFINITE)
	var code uint32
	err := syscall.GetExitCodeProcess(s.process, &code)
	syscall.CloseHandle(s.thread)
	syscall.CloseHandle(s.process)
	if err != nil {
		return -1
	}
	return int(int32(code))
}

// resizePTY, close, and wait above intentionally do not live in
// pty_unix_common.go: Windows' ptySession has a different shape (a pipe
// pair plus a console handle, not a single *os.File master), so none of
// that shared Unix code applies here.
