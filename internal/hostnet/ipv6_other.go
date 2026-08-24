//go:build !linux

package hostnet

// Not implemented off Linux, and not silently: every platform here generates
// a link-local when an interface comes up and none of them exposes the
// per-interface forwarding knob procfs does, so there is nothing to assert
// and no evidence of a host that needs it. Stubs rather than a build tag at
// the call site, so Apply reads the same on every platform.

func ensureLinkLocal6(string) (bool, error) { return false, nil }

func ensureForwarding6(string) error { return nil }
