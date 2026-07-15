//go:build freebsd || openbsd

// Package ipfwd toggles host IP forwarding so a gravinet node can route
// traffic between the overlay and other interfaces (the on-ramp for
// redistributed routes and NAT). On FreeBSD this shells out to sysctl(8),
// which ships with the base system: FreeBSD doesn't reliably have /proc
// mounted the way Linux always does (procfs is optional and off by default
// on modern FreeBSD), so the direct-file-read/write approach ipfwd_linux.go
// uses isn't available here, and going through the base-system CLI tool
// avoids pulling in a raw sysctl(3)/sysctlbyname(3) binding for it.
package ipfwd

import (
	"os/exec"
	"strings"
)

// sysctl names; vars (not consts) so tests can redirect them.
var (
	sysctlV4 = "net.inet.ip.forwarding"
	sysctlV6 = "net.inet6.ip6.forwarding"
)

// State records the forwarding values seen before Enable changed them, so
// they can be put back by Restore. Only knobs that were successfully read
// are tracked. Same shape as every other platform's ipfwd.State — the fields
// main.go reads (V4Failed, V6Failed) and the methods it calls (V4Missing,
// V6Missing) are identical across all of them.
type State struct {
	v4set, v6set       bool
	v4prior, v6prior   string
	v4miss, v6miss     bool // knob absent (unlikely on either BSD, but sysctl -n fails cleanly if so)
	V4Failed, V6Failed bool // could not write (e.g. no privilege)
}

func readSysctl(name string) (string, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func writeSysctl(name, val string) error {
	return exec.Command("sysctl", name+"="+val).Run()
}

// Enable turns on IPv4 and/or IPv6 forwarding. It is best-effort and family-
// independent: a missing knob or a write failure on one family does not stop
// the other. The returned State drives Restore; the bool fields report what
// couldn't be done so the caller can log it.
func Enable(v4, v6 bool) State {
	var st State
	if v4 {
		if prior, err := readSysctl(sysctlV4); err == nil {
			st.v4set, st.v4prior = true, prior
			if prior != "1" {
				if err := writeSysctl(sysctlV4, "1"); err != nil {
					st.V4Failed = true
				}
			}
		} else {
			st.v4miss = true
		}
	}
	if v6 {
		if prior, err := readSysctl(sysctlV6); err == nil {
			st.v6set, st.v6prior = true, prior
			if prior != "1" {
				if err := writeSysctl(sysctlV6, "1"); err != nil {
					st.V6Failed = true
				}
			}
		} else {
			st.v6miss = true
		}
	}
	return st
}

// V4Missing / V6Missing report that a knob wasn't present (family disabled
// or, on some minimal installs, the corresponding module unloaded).
func (s State) V4Missing() bool { return s.v4miss }
func (s State) V6Missing() bool { return s.v6miss }

// Restore reverts forwarding to the values captured by Enable. It only
// writes knobs Enable actually read, so a setting that was already on (and
// that gravinet merely left on) is restored to on, and one gravinet flipped
// from off→on is reverted to off. Best-effort; errors are ignored (shutdown
// path).
func Restore(st State) {
	if st.v4set {
		_ = writeSysctl(sysctlV4, st.v4prior)
	}
	if st.v6set {
		_ = writeSysctl(sysctlV6, st.v6prior)
	}
}
