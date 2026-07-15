//go:build linux

// Package ipfwd toggles host IP forwarding so a gravinet node can route traffic
// between the overlay and other interfaces (the on-ramp for redistributed routes
// and NAT). On Linux it writes the procfs sysctl knobs directly — no `sysctl`
// binary, no cgo.
package ipfwd

import (
	"os"
	"strings"
)

// proc paths; vars (not consts) so tests can redirect them to temp files.
var (
	procV4 = "/proc/sys/net/ipv4/ip_forward"
	procV6 = "/proc/sys/net/ipv6/conf/all/forwarding"
)

// State records the forwarding values seen before Enable changed them, so they
// can be put back by Restore. Only knobs that were successfully read are tracked.
type State struct {
	v4set, v6set       bool
	v4prior, v6prior   string
	v4miss, v6miss     bool // knob absent (e.g. IPv6 disabled)
	V4Failed, V6Failed bool // could not write (e.g. read-only, no privilege)
}

func readVal(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeVal(path, val string) error {
	return os.WriteFile(path, []byte(val+"\n"), 0o644)
}

// Enable turns on IPv4 and/or IPv6 forwarding. It is best-effort and family-
// independent: a missing knob (IPv6 disabled) or a write failure on one family
// does not stop the other. The returned State drives Restore; the bool fields
// report what couldn't be done so the caller can log it.
func Enable(v4, v6 bool) State {
	var st State
	if v4 {
		if prior, err := readVal(procV4); err == nil {
			st.v4set, st.v4prior = true, prior
			if prior != "1" {
				if err := writeVal(procV4, "1"); err != nil {
					st.V4Failed = true
				}
			}
		} else if os.IsNotExist(err) {
			st.v4miss = true
		} else {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, err := readVal(procV6); err == nil {
			st.v6set, st.v6prior = true, prior
			if prior != "1" {
				if err := writeVal(procV6, "1"); err != nil {
					st.V6Failed = true
				}
			}
		} else if os.IsNotExist(err) {
			st.v6miss = true
		} else {
			st.V6Failed = true
		}
	}
	return st
}

// V4Missing / V6Missing report that a knob wasn't present (family disabled).
func (s State) V4Missing() bool { return s.v4miss }
func (s State) V6Missing() bool { return s.v6miss }

// Restore reverts forwarding to the values captured by Enable. It only writes
// knobs Enable actually read, so a setting that was already on (and that gravinet
// merely left on) is restored to on, and one gravinet flipped from off→on is
// reverted to off. Best-effort; errors are ignored (shutdown path).
func Restore(st State) {
	if st.v4set {
		_ = writeVal(procV4, st.v4prior)
	}
	if st.v6set {
		_ = writeVal(procV6, st.v6prior)
	}
}
