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

// redirect proc paths; vars (not consts) so tests can redirect them to temp
// files. IPv4 exposes both an accept and a send knob — a node that forwards
// (see Enable above) can itself be told "send your traffic through me
// instead" by an unauthenticated redirect, so both directions matter here,
// not just acceptance. IPv6 only has an accept knob; the kernel has no
// per-host send_redirects for v6. Only the "all" scope is touched, same as
// procV4/procV6 above: gravinet doesn't bring up new interfaces before this
// runs at startup, so "default" (which only affects interfaces added *after*
// it's set) wouldn't cover anything "all" doesn't already.
var (
	procV4AcceptRedirects = "/proc/sys/net/ipv4/conf/all/accept_redirects"
	procV4SendRedirects   = "/proc/sys/net/ipv4/conf/all/send_redirects"
	procV6AcceptRedirects = "/proc/sys/net/ipv6/conf/all/accept_redirects"
)

// RedirectState records the redirect-knob values seen before DisableRedirects
// changed them, so they can be put back by RestoreRedirects. Only knobs that
// were successfully read are tracked — same shape and reasoning as State above.
type RedirectState struct {
	v4AcceptSet, v4SendSet, v6AcceptSet       bool
	v4AcceptPrior, v4SendPrior, v6AcceptPrior string
	v4miss, v6miss                            bool // knob absent (e.g. IPv6 disabled)
	V4Failed, V6Failed                        bool // could not write (e.g. read-only, no privilege)
}

// DisableRedirects turns off IPv4 and/or IPv6 ICMP redirect acceptance (and,
// for IPv4, sending) so an unauthenticated ICMP redirect can't rewrite this
// host's route table. Best-effort and family-independent, same contract as
// Enable: a missing knob or a write failure on one family/knob doesn't stop
// the others. The returned RedirectState drives RestoreRedirects; the bool
// fields report what couldn't be done so the caller can log it.
func DisableRedirects(v4, v6 bool) RedirectState {
	var st RedirectState
	if v4 {
		if prior, err := readVal(procV4AcceptRedirects); err == nil {
			st.v4AcceptSet, st.v4AcceptPrior = true, prior
			if prior != "0" {
				if err := writeVal(procV4AcceptRedirects, "0"); err != nil {
					st.V4Failed = true
				}
			}
		} else if os.IsNotExist(err) {
			st.v4miss = true
		} else {
			st.V4Failed = true
		}
		if prior, err := readVal(procV4SendRedirects); err == nil {
			st.v4SendSet, st.v4SendPrior = true, prior
			if prior != "0" {
				if err := writeVal(procV4SendRedirects, "0"); err != nil {
					st.V4Failed = true
				}
			}
		} else if !os.IsNotExist(err) {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, err := readVal(procV6AcceptRedirects); err == nil {
			st.v6AcceptSet, st.v6AcceptPrior = true, prior
			if prior != "0" {
				if err := writeVal(procV6AcceptRedirects, "0"); err != nil {
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

// V4Missing / V6Missing report that a redirect knob wasn't present (family
// disabled).
func (s RedirectState) V4Missing() bool { return s.v4miss }
func (s RedirectState) V6Missing() bool { return s.v6miss }

// RestoreRedirects reverts the redirect knobs to the values captured by
// DisableRedirects, same "only touch what was actually read" contract as
// Restore above. Best-effort; errors are ignored (shutdown path).
func RestoreRedirects(st RedirectState) {
	if st.v4AcceptSet {
		_ = writeVal(procV4AcceptRedirects, st.v4AcceptPrior)
	}
	if st.v4SendSet {
		_ = writeVal(procV4SendRedirects, st.v4SendPrior)
	}
	if st.v6AcceptSet {
		_ = writeVal(procV6AcceptRedirects, st.v6AcceptPrior)
	}
}
