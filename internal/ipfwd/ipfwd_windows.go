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
