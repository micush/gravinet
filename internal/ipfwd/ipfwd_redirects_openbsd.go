//go:build openbsd

// Redirect-disabling lives in its own per-OS file — see
// ipfwd_redirects_freebsd.go's doc comment for why this isn't shared with
// FreeBSD despite ipfwd_bsd.go sharing forwarding Enable/Restore across both.
// Reuses readSysctl/writeSysctl from ipfwd_bsd.go (same package, same build).
package ipfwd

// Redirect sysctl names. Unlike FreeBSD, OpenBSD's accept knobs are not
// inverted: net.inet.icmp.rediraccept and net.inet6.icmp6.rediraccept are
// both 1=accepted (the usual sense), so unlike ipfwd_redirects_freebsd.go
// every knob here disables with a plain "0".
var (
	sysctlV4Send   = "net.inet.ip.redirect"
	sysctlV4Accept = "net.inet.icmp.rediraccept"
	sysctlV6Send   = "net.inet6.ip6.redirect"
	sysctlV6Accept = "net.inet6.icmp6.rediraccept"
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
		if prior, err := readSysctl(sysctlV4Accept); err == nil {
			st.v4AccSet, st.v4AccPrior = true, prior
			if prior != "0" {
				if writeSysctl(sysctlV4Accept, "0") != nil {
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
// wasn't present (unlikely on OpenBSD, but sysctl -n fails cleanly if so).
func (s RedirectState) V4Missing() bool { return s.v4miss }
func (s RedirectState) V6Missing() bool { return s.v6miss }

// RestoreRedirects reverts the redirect sysctls to their captured prior
// values. Best-effort; errors are ignored (shutdown path).
func RestoreRedirects(st RedirectState) {
	if st.v4SendSet {
		_ = writeSysctl(sysctlV4Send, st.v4SendPrior)
	}
	if st.v4AccSet {
		_ = writeSysctl(sysctlV4Accept, st.v4AccPrior)
	}
	if st.v6SendSet {
		_ = writeSysctl(sysctlV6Send, st.v6SendPrior)
	}
	if st.v6AccSet {
		_ = writeSysctl(sysctlV6Accept, st.v6AccPrior)
	}
}
