//go:build windows

package ipfwd

import (
	"os/exec"
	"strings"
)

// Windows global IP routing is controlled by the IPEnableRouter registry value
// for IPv4 and IPv6. Changing it generally requires the Routing service to
// restart (or a reboot) to take full effect; this is best-effort and untested
// on this build host.
const (
	regV4  = `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`
	regV6  = `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters`
	regVal = "IPEnableRouter"
)

// State records prior registry values for restoration.
type State struct {
	v4set, v6set     bool
	v4prior, v6prior string
	V4Failed         bool
	V6Failed         bool
}

func regGet(path string) (string, bool) {
	out, err := exec.Command("reg", "query", path, "/v", regVal).Output()
	if err != nil {
		return "", false
	}
	// Output looks like: "    IPEnableRouter    REG_DWORD    0x1"
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if strings.EqualFold(f, regVal) && i+2 < len(fields) {
			v := fields[i+2]
			if strings.HasPrefix(v, "0x") && v != "0x0" {
				return "1", true
			}
			return "0", true
		}
	}
	return "0", true
}

func regSet(path, val string) error {
	return exec.Command("reg", "add", path, "/v", regVal, "/t", "REG_DWORD", "/d", val, "/f").Run()
}

// Enable sets IPEnableRouter=1 for the requested families, recording prior values.
func Enable(v4, v6 bool) State {
	var st State
	if v4 {
		prior, _ := regGet(regV4)
		st.v4set, st.v4prior = true, prior
		if prior != "1" && regSet(regV4, "1") != nil {
			st.V4Failed = true
		}
	}
	if v6 {
		prior, _ := regGet(regV6)
		st.v6set, st.v6prior = true, prior
		if prior != "1" && regSet(regV6, "1") != nil {
			st.V6Failed = true
		}
	}
	return st
}

func (s State) V4Missing() bool { return false }
func (s State) V6Missing() bool { return false }

// Restore reverts IPEnableRouter to the captured prior values. Best-effort.
func Restore(st State) {
	if st.v4set {
		_ = regSet(regV4, st.v4prior)
	}
	if st.v6set {
		_ = regSet(regV6, st.v6prior)
	}
}

// ICMP redirect handling is a per-protocol *global* netsh setting on Windows
// (no registry value backs it the way IPEnableRouter does above), toggled via
// `netsh interface {ipv4,ipv6} set global icmpredirects=enabled|disabled` and
// read back via the matching `show global`. Untested on this build host, same
// caveat as Enable/Restore above.
var redirectsShowGlobalArgs = map[bool][]string{
	true:  {"interface", "ipv4", "show", "global"},
	false: {"interface", "ipv6", "show", "global"},
}

func netshRedirectsGet(v4 bool) (string, bool) {
	out, err := exec.Command("netsh", redirectsShowGlobalArgs[v4]...).Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(strings.ToLower(line), "icmp redirects") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "disabled") {
			return "disabled", true
		}
		if strings.Contains(strings.ToLower(line), "enabled") {
			return "enabled", true
		}
	}
	return "", false
}

func netshRedirectsSet(v4 bool, val string) error {
	proto := "ipv6"
	if v4 {
		proto = "ipv4"
	}
	return exec.Command("netsh", "interface", proto, "set", "global", "icmpredirects="+val).Run()
}

// RedirectState records prior netsh global values for RestoreRedirects.
type RedirectState struct {
	v4set, v6set     bool
	v4prior, v6prior string
	V4Failed         bool
	V6Failed         bool
}

// DisableRedirects sets icmpredirects=disabled for the requested protocols,
// recording prior values. Best-effort.
func DisableRedirects(v4, v6 bool) RedirectState {
	var st RedirectState
	if v4 {
		if prior, ok := netshRedirectsGet(true); ok {
			st.v4set, st.v4prior = true, prior
			if prior != "disabled" && netshRedirectsSet(true, "disabled") != nil {
				st.V4Failed = true
			}
		} else {
			st.V4Failed = true
		}
	}
	if v6 {
		if prior, ok := netshRedirectsGet(false); ok {
			st.v6set, st.v6prior = true, prior
			if prior != "disabled" && netshRedirectsSet(false, "disabled") != nil {
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

// RestoreRedirects reverts icmpredirects to its captured prior value.
// Best-effort.
func RestoreRedirects(st RedirectState) {
	if st.v4set {
		_ = netshRedirectsSet(true, st.v4prior)
	}
	if st.v6set {
		_ = netshRedirectsSet(false, st.v6prior)
	}
}
