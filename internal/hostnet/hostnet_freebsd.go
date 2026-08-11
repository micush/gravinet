//go:build freebsd

package hostnet

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// FreeBSD keeps boot-time addressing in /etc/rc.conf. Written through
// sysrc(8) rather than by editing the file: sysrc is the supported editor,
// it preserves everything else in there, and hand-editing a file this
// central is how a host comes back without networking.
func detect() *Backend {
	if _, err := exec.LookPath("sysrc"); err != nil {
		return nil
	}
	return &Backend{Name: "rc.conf", Persist: persistRcConf}
}

func persistRcConf(s Spec) error {
	v4, v6 := v4v6(s.Addrs)

	// The first address is ifconfig_<if>; the rest are ifconfig_<if>_aliasN,
	// which is rc.conf's own convention for multiple addresses.
	if len(v4) == 0 {
		if err := sysrcDel("ifconfig_" + s.Iface); err != nil {
			return err
		}
	} else {
		line := "inet " + v4[0].String()
		if s.MTU > 0 {
			line += " mtu " + strconv.Itoa(s.MTU)
		}
		if err := sysrcSet("ifconfig_"+s.Iface, line); err != nil {
			return err
		}
		for i, p := range v4[1:] {
			if err := sysrcSet(fmt.Sprintf("ifconfig_%s_alias%d", s.Iface, i), "inet "+p.String()); err != nil {
				return err
			}
		}
	}
	for i, p := range v6 {
		key := "ifconfig_" + s.Iface + "_ipv6"
		if i > 0 {
			key = fmt.Sprintf("ifconfig_%s_alias%d", s.Iface, len(v4)+i-1)
		}
		if err := sysrcSet(key, "inet6 "+p.String()); err != nil {
			return err
		}
	}
	// An interface with no IPv4 address still needs its MTU recorded, and
	// rc.conf has nowhere else to put it.
	if len(v4) == 0 && s.MTU > 0 {
		if err := sysrcSet("ifconfig_"+s.Iface, "mtu "+strconv.Itoa(s.MTU)); err != nil {
			return err
		}
	}
	if s.GW4.IsValid() {
		if err := sysrcSet("defaultrouter", s.GW4.String()); err != nil {
			return err
		}
	}
	if s.GW6.IsValid() {
		if err := sysrcSet("ipv6_defaultrouter", s.GW6.String()); err != nil {
			return err
		}
	}
	return nil
}

func sysrcSet(key, val string) error {
	out, err := exec.Command("sysrc", key+"="+val).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysrc %s: %v (%s)", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sysrcDel tolerates the key already being absent, which is not an error.
func sysrcDel(key string) error {
	_ = exec.Command("sysrc", "-x", key).Run()
	return nil
}
