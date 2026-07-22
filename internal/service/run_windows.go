//go:build windows

package service

import (
	"os/exec"
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

	// svcRestartExitCode is the non-zero exit code we report when stopping for
	// a restart, so the SCM treats it as a failure and runs the configured
	// recovery action (restart) — rather than the deadlock-prone approach of
	// the service calling Restart-Service on itself. Any non-zero value works;
	// this one is arbitrary but distinctive in the event log.
	svcRestartExitCode = 42
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
	runningUnderSCM bool   // set once serviceMainCb runs (i.e. the SCM dispatched us)
	svcExitCode     uint32 // reported in the final SERVICE_STOPPED; non-zero triggers recovery
)

// report sets a running/pending state with no exit code.
func report(state, accept uint32) {
	st := serviceStatus{ServiceType: serviceWin32OwnProcess, CurrentState: state, ControlsAccepted: accept}
	procSetServiceStatus.Call(svcStatusHandle, uintptr(unsafe.Pointer(&st)))
}

// reportStopped reports SERVICE_STOPPED with an exit code. A non-zero code, in
// combination with the failure-actions flag set at install time (and by
// ensureRecoveryActions), makes the SCM run the configured recovery action.
func reportStopped(exitCode uint32) {
	st := serviceStatus{ServiceType: serviceWin32OwnProcess, CurrentState: serviceStopped, Win32ExitCode: exitCode}
	procSetServiceStatus.Call(svcStatusHandle, uintptr(unsafe.Pointer(&st)))
}

// RestartViaServiceManagerExit arranges for a restart to happen through the
// SCM's recovery action rather than by re-execing or self-Restart-Service. On
// Windows, when running under the SCM, it arms a non-zero service exit code and
// returns true — the caller then simply returns from the service function, and
// the SCM restarts us. It returns false when not running as a managed service
// (interactive run), so the caller falls back to its normal restart path.
//
// This is the fix for the Windows "settings change stops the service and it
// never comes back" bug: the service must not call Restart-Service on itself
// (that deadlocks against its own SCM stop, leaving it stopped); reporting a
// failure exit and letting the recovery action restart us is the supported way.
func RestartViaServiceManagerExit() bool {
	if !runningUnderSCM {
		return false
	}
	svcExitCode = svcRestartExitCode
	return true
}

// ensureRecoveryActions best-effort-configures the service's failure/recovery
// actions so it auto-restarts on failure, and sets the failure-actions flag so
// those actions also fire on a non-zero-exit stop (like our restart exit).
// Shelling out to sc.exe avoids marshaling SERVICE_FAILURE_ACTIONS by hand and
// is idempotent, so it also repairs an existing install that was created
// without recovery actions. Runs as the service account (LocalSystem), which
// has the rights to change service config. Errors are ignored: recovery is a
// safety net, not a startup prerequisite.
func ensureRecoveryActions(name string) {
	// reset= 86400: forget the failure count after a day of stability.
	// actions: restart on the first, second, and subsequent failures, with a
	// short escalating backoff (5s, 10s, 30s) so a persistent crash-loop backs
	// off instead of hammering.
	_ = exec.Command("sc.exe", "failure", name, "reset=", "86400",
		"actions=", "restart/5000/restart/10000/restart/30000").Run()
	_ = exec.Command("sc.exe", "failureflag", name, "1").Run()
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
	runningUnderSCM = true
	namePtr, _ := syscall.UTF16PtrFromString(svcName)
	h, _, _ := procRegisterHandlerEx.Call(
		uintptr(unsafe.Pointer(namePtr)),
		syscall.NewCallback(handlerCb),
		0,
	)
	svcStatusHandle = h
	report(serviceRunning, serviceAcceptStop|serviceAcceptShutdown)
	// Make sure recovery actions are configured (idempotent; repairs older
	// installs). Done after the RUNNING ack and off the main path so it can't
	// delay startup past the SCM's timeout.
	go ensureRecoveryActions(svcName)
	svcRun(svcStop)
	// Report the final state with whatever exit code was armed. 0 for an
	// ordinary stop (no recovery); non-zero when we're exiting to be restarted
	// by the recovery action.
	reportStopped(svcExitCode)
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
