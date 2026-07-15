//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package transport

import "syscall"

// reusePort is false off Linux; we open one socket per family and spread the
// worker goroutines across it instead.
const reusePort = false

// control is a no-op placeholder on platforms with no dedicated
// implementation here (everything except linux, darwin, freebsd, openbsd,
// and windows — see control_linux.go, control_darwin.go, control_freebsd.go,
// control_openbsd.go, and control_windows.go).
func control(network, address string, c syscall.RawConn) error { return nil }
