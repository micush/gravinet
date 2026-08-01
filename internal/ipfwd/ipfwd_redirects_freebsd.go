//go:build freebsd

// Redirect-disabling lives in its own per-OS file, split out of
// ipfwd_bsd.go's shared freebsd||openbsd build, because FreeBSD and OpenBSD
// disagree on both the sysctl name *and* the polarity of the accept-redirect
// knob: FreeBSD's net.inet.icmp.drop_redirect is inverted (1 = redirects are
// dropped, i.e. accept is disabled) while OpenBSD's net.inet.icmp.rediraccept
// is not (1 = accepted, the usual sense) — see ipfwd_redirects_openbsd.go.
// Reuses readSysctl/writeSysctl from ipfwd_bsd.go (same package, same build).
package ipfwd

// Redirect sysctl names. IPv4 has a send knob (net.inet.ip.redirect, 1=send,
// usual sense) and an inverted accept knob (net.inet.icmp.drop_redirect,
// 1=drop incoming redirects i.e. accept disabled). IPv6 has both, neither
// inverted: net.inet6.ip6.redirect (send) and net.inet6.icmp6.rediraccept
// (accept).
var (
	sysctlV4Send      = "net.inet.ip.redirect"
	sysctlV4DropOnAcc = "net.inet.icmp.drop_redirect" // inverted: write "1" to disable accept
	sysctlV6Send      = "net.inet6.ip6.redirect"
	sysctlV6Accept    = "net.inet6.icmp6.rediraccept"
)

// RedirectState records prior sysctl values for RestoreRedirects. Only knobs
// that were successfully read are tracked.
type RedirectState struct {
	v4SendSet, v4AccSet, v6SendSet, v6AccSet         bool
	v4SendPrior, v4AccPrior, v6SendPrior, v6AccPrior string
	v4miss, v6miss                                   bool
	V4Failed, V6Failed                               bool
}

// DisableRedirects turns off IPv4/IPv6 ICMP redirect sending and acceptance
// via sysctl(8), recording prior values. Best-effort and knob-independent,
// same contract as Enable in ipfwd_bsd.go.
func DisableRedirects(v4, v6 bool) RedirectState {
	var st RedirectState
	if v4 {
		if prior, err := readSysctl(sysctlV4Send); err == nil {
			st.v4SendSet, st.v4SendPrior = true, prior
			if prior != "0" {
				if writeSysctl(sysctlV4Send, "0") != nil {
					st.V4Failed = true
				}
			}
		} else {
			st.v4miss = true
		}
		if prior, err := readSysctl(sysctlV4DropOnAcc); err == nil {
			st.v4AccSet, st.v4AccPrior = true, prior
			if prior != "1" { // inverted: "1" means accept is disabled
				if writeSysctl(sysctlV4DropOnAcc, "1") != nil {
					st.V4Failed = true
				}
			}
		} else {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, err := readSysctl(sysctlV6Send); err == nil {
			st.v6SendSet, st.v6SendPrior = true, prior
			if prior != "0" {
				if writeSysctl(sysctlV6Send, "0") != nil {
					st.V6Failed = true
				}
			}
		} else {
			st.v6miss = true
		}
		if prior, err := readSysctl(sysctlV6Accept); err == nil {
			st.v6AccSet, st.v6AccPrior = true, prior
			if prior != "0" {
				if writeSysctl(sysctlV6Accept, "0") != nil {
					st.V6Failed = true
				}
			}
		} else {
			st.V6Failed = true
		}
	}
	return st
}

// V4Missing / V6Missing report that the send-redirect knob for that family
// wasn't present (unlikely on FreeBSD, but sysctl -n fails cleanly if so).
func (s RedirectState) V4Missing() bool { return s.v4miss }
func (s RedirectState) V6Missing() bool { return s.v6miss }

// RestoreRedirects reverts the redirect sysctls to their captured prior
// values. Best-effort; errors are ignored (shutdown path).
func RestoreRedirects(st RedirectState) {
	if st.v4SendSet {
		_ = writeSysctl(sysctlV4Send, st.v4SendPrior)
	}
	if st.v4AccSet {
		_ = writeSysctl(sysctlV4DropOnAcc, st.v4AccPrior)
	}
	if st.v6SendSet {
		_ = writeSysctl(sysctlV6Send, st.v6SendPrior)
	}
	if st.v6AccSet {
		_ = writeSysctl(sysctlV6Accept, st.v6AccPrior)
	}
}
