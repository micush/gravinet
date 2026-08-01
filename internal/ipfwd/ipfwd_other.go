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

// RedirectState is a no-op placeholder on platforms without a redirect
// backend.
type RedirectState struct {
	V4Failed bool
	V6Failed bool
}

// DisableRedirects is a no-op; redirect handling must be configured by the
// operator.
func DisableRedirects(v4, v6 bool) RedirectState { return RedirectState{} }

func (s RedirectState) V4Missing() bool { return false }
func (s RedirectState) V6Missing() bool { return false }

// RestoreRedirects is a no-op.
func RestoreRedirects(st RedirectState) {}
