//go:build windows

package hostnet

// Windows persists by default: netsh's store defaults to "persistent", so the
// commands the apply path already runs are written to the registry as well as
// to the running stack. There is nothing further to do, and saying "no
// backend" here would be wrong in the direction that matters — it would tell
// an operator their change is temporary when it is not.
func detect() *Backend {
	return &Backend{
		Name:    "netsh (persistent)",
		Persist: func(Spec) error { return nil },
	}
}
