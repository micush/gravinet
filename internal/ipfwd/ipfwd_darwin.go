//go:build darwin

package ipfwd

import (
	"os/exec"
	"strings"
)

const (
	keyV4 = "net.inet.ip.forwarding"
	keyV6 = "net.inet6.ip6.forwarding"
)

// State records prior sysctl values for restoration.
type State struct {
	v4set, v6set     bool
	v4prior, v6prior string
	V4Failed         bool
	V6Failed         bool
}

func sysctlGet(key string) (string, bool) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func sysctlSet(key, val string) error {
	return exec.Command("sysctl", "-w", key+"="+val).Run()
}

// Enable turns on forwarding via sysctl, recording prior values. Best-effort.
func Enable(v4, v6 bool) State {
	var st State
	if v4 {
		if prior, ok := sysctlGet(keyV4); ok {
			st.v4set, st.v4prior = true, prior
			if prior != "1" && sysctlSet(keyV4, "1") != nil {
				st.V4Failed = true
			}
		} else {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, ok := sysctlGet(keyV6); ok {
			st.v6set, st.v6prior = true, prior
			if prior != "1" && sysctlSet(keyV6, "1") != nil {
				st.V6Failed = true
			}
		} else {
			st.V6Failed = true
		}
	}
	return st
}

func (s State) V4Missing() bool { return false }
func (s State) V6Missing() bool { return false }

// Restore reverts forwarding to the captured prior values. Best-effort.
func Restore(st State) {
	if st.v4set {
		_ = sysctlSet(keyV4, st.v4prior)
	}
	if st.v6set {
		_ = sysctlSet(keyV6, st.v6prior)
	}
}

// Redirect sysctl keys. IPv4 has a send knob (net.inet.ip.redirect, 1=send,
// the usual sense) and an accept knob that's inverted (net.inet.icmp.drop_redirect,
// 1=drop incoming redirects i.e. don't accept them) — macOS actually ships
// with drop_redirect=1 out of the box, so redirectV4AcceptTarget below is
// "1", not "0" like every other knob here. IPv6 only exposes a send knob
// (net.inet6.ip6.redirect); this build has no confirmed per-host IPv6
// accept-redirect sysctl to pair with it.
const (
	keyV4Send      = "net.inet.ip.redirect"
	keyV4Accept    = "net.inet.icmp.drop_redirect"
	keyV6Send      = "net.inet6.ip6.redirect"
	redirectOffVal = "0"
	dropOnVal      = "1" // drop_redirect: 1 means redirects are dropped (accept disabled)
)

// RedirectState records prior sysctl values for RestoreRedirects.
type RedirectState struct {
	v4SendSet, v4AcceptSet, v6SendSet       bool
	v4SendPrior, v4AcceptPrior, v6SendPrior string
	V4Failed                                bool
	V6Failed                                bool
}

// DisableRedirects turns off IPv4 send+accept and IPv6 send of ICMP
// redirects via sysctl, recording prior values. Best-effort.
func DisableRedirects(v4, v6 bool) RedirectState {
	var st RedirectState
	if v4 {
		if prior, ok := sysctlGet(keyV4Send); ok {
			st.v4SendSet, st.v4SendPrior = true, prior
			if prior != redirectOffVal && sysctlSet(keyV4Send, redirectOffVal) != nil {
				st.V4Failed = true
			}
		} else {
			st.V4Failed = true
		}
		if prior, ok := sysctlGet(keyV4Accept); ok {
			st.v4AcceptSet, st.v4AcceptPrior = true, prior
			if prior != dropOnVal && sysctlSet(keyV4Accept, dropOnVal) != nil {
				st.V4Failed = true
			}
		} else {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, ok := sysctlGet(keyV6Send); ok {
			st.v6SendSet, st.v6SendPrior = true, prior
			if prior != redirectOffVal && sysctlSet(keyV6Send, redirectOffVal) != nil {
				st.V6Failed = true
			}
		} else {
			st.V6Failed = true
		}
	}
	return st
}

func (s RedirectState) V4Missing() bool { return false }
func (s RedirectState) V6Missing() bool { return false }

// RestoreRedirects reverts the redirect sysctls to their captured prior
// values. Best-effort.
func RestoreRedirects(st RedirectState) {
	if st.v4SendSet {
		_ = sysctlSet(keyV4Send, st.v4SendPrior)
	}
	if st.v4AcceptSet {
		_ = sysctlSet(keyV4Accept, st.v4AcceptPrior)
	}
	if st.v6SendSet {
		_ = sysctlSet(keyV6Send, st.v6SendPrior)
	}
}
