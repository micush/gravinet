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
