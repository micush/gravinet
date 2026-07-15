//go:build windows

package service

import (
	"syscall"
	"unsafe"
)

// This is the Windows service-control-manager integration. It talks to the SCM
// through advapi32 with no external dependency (no golang.org/x/sys), using
// syscall.NewCallback for the service entry point and control handler. It
// compiles for windows/amd64; it cannot be exercised on non-Windows hosts.

var (
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procStartDispatcher   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterHandlerEx = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus  = advapi32.NewProc("SetServiceStatus")
)

const (
	serviceWin32OwnProcess = 0x10
	serviceStopped         = 1
	serviceStopPending     = 3
	serviceRunning         = 4
	serviceAcceptStop      = 1
	serviceAcceptShutdown  = 4
	controlStop            = 1
	controlShutdown        = 5
	errFailedConnect       = 1063
)

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

type serviceTableEntry struct {
	name *uint16
	proc uintptr
}

var (
	svcName         string
	svcRun          func(stop <-chan struct{})
	svcStop         chan struct{}
	svcDone         chan struct{}
	svcStatusHandle uintptr
)

func report(state, accept uint32) {
	st := serviceStatus{ServiceType: serviceWin32OwnProcess, CurrentState: state, ControlsAccepted: accept}
	procSetServiceStatus.Call(svcStatusHandle, uintptr(unsafe.Pointer(&st)))
}

// handlerCb matches LPHANDLER_FUNCTION_EX.
func handlerCb(control, eventType, eventData, context uintptr) uintptr {
	switch uint32(control) {
	case controlStop, controlShutdown:
		report(serviceStopPending, 0)
		select {
		case <-svcStop:
		default:
			close(svcStop)
		}
	}
	return 0
}

// serviceMainCb matches LPSERVICE_MAIN_FUNCTION.
func serviceMainCb(argc uint32, argv **uint16) uintptr {
	namePtr, _ := syscall.UTF16PtrFromString(svcName)
	h, _, _ := procRegisterHandlerEx.Call(
		uintptr(unsafe.Pointer(namePtr)),
		syscall.NewCallback(handlerCb),
		0,
	)
	svcStatusHandle = h
	report(serviceRunning, serviceAcceptStop|serviceAcceptShutdown)
	svcRun(svcStop)
	report(serviceStopped, 0)
	close(svcDone)
	return 0
}

// RunService attempts to run under the SCM. It returns (true, nil) when the
// process was started as a service (blocking until stopped), or (false, nil)
// when it was launched interactively, so the caller can run normally.
func RunService(name string, run func(stop <-chan struct{})) (bool, error) {
	svcName = name
	svcRun = run
	svcStop = make(chan struct{})
	svcDone = make(chan struct{})

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	table := []serviceTableEntry{
		{name: namePtr, proc: syscall.NewCallback(serviceMainCb)},
		{name: nil, proc: 0},
	}
	r, _, callErr := procStartDispatcher.Call(uintptr(unsafe.Pointer(&table[0])))
	if r == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && uint32(errno) == errFailedConnect {
			return false, nil // not started by the SCM (interactive run)
		}
		return false, callErr
	}
	<-svcDone
	return true, nil
}
