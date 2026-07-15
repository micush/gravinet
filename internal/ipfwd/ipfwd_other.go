//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package ipfwd

// State is a no-op placeholder on platforms without a forwarding backend.
type State struct {
	V4Failed bool
	V6Failed bool
}

// Enable is a no-op; forwarding must be configured by the operator.
func Enable(v4, v6 bool) State { return State{} }

func (s State) V4Missing() bool { return false }
func (s State) V6Missing() bool { return false }

// Restore is a no-op.
func Restore(st State) {}
