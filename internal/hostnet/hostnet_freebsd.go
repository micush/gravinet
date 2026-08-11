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
// Reapply is left nil. FreeBSD's per-interface reload is
// `/etc/rc.d/netif restart <if>`, which takes the link down and back up —
// including the link carrying the request that asked for it. The operator is
// told the mode is written and takes effect on the next boot, which is a fact
// they can act on, rather than having their session dropped to save them a
// reboot they may not have wanted yet.
func detect() *Backend {
	if _, err := exec.LookPath("sysrc"); err != nil {
		return nil
	}
	return &Backend{Name: "rc.conf", Persist: persistRcConf}
}

func persistRcConf(s Spec) error {
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	if mode6 == ModeDHCP6 {
		return errNoDHCPv6("FreeBSD", "install dhcp6c from ports and configure it directly, or use slaac")
	}
	v4, v6 := v4v6(s.StaticAddrs(s.Addrs))

	if mode4 == ModeDHCP {
		// rc.conf's spelling for "run the DHCP client on this interface". The
		// MTU rides along on the same line, which is rc.d/netif's documented
		// handling of options alongside DHCP — it has nowhere else to go, as
		// there is no separate ifconfig_<if>_mtu.
		line := "DHCP"
		if s.MTU > 0 {
			line += " mtu " + strconv.Itoa(s.MTU)
		}
		if err := sysrcSet("ifconfig_"+s.Iface, line); err != nil {
			return err
		}
	} else if err := persistRcConfStatic4(s, v4); err != nil {
		return err
	}

	if mode6 == ModeSLAAC {
		if err := sysrcSet("ifconfig_"+s.Iface+"_ipv6", "inet6 accept_rtadv"); err != nil {
			return err
		}
		// accept_rtadv on its own is a silent no-op on FreeBSD: the kernel
		// accepts advertisements but nothing solicits one, so an interface
		// brought up before the router's next unsolicited RA sits without an
		// address for up to ten minutes. rtsold is what solicits, and this is
		// the one host-wide knob this backend sets — named here because it is
		// the only place in this package that reaches beyond the interface it
		// was asked about.
		if err := sysrcSet("rtsold_enable", "YES"); err != nil {
			return err
		}
	} else if err := persistRcConfStatic6(s, v4, v6); err != nil {
		return err
	}

	if s.GW4.IsValid() && mode4 == ModeStatic {
		if err := sysrcSet("defaultrouter", s.GW4.String()); err != nil {
			return err
		}
	}
	if s.GW6.IsValid() && mode6 == ModeStatic {
		if err := sysrcSet("ipv6_defaultrouter", s.GW6.String()); err != nil {
			return err
		}
	}
	return nil
}

func persistRcConfStatic4(s Spec, v4 []netipPrefix) error {
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
	// An interface with no IPv4 address still needs its MTU recorded, and
	// rc.conf has nowhere else to put it.
	if len(v4) == 0 && s.MTU > 0 {
		if err := sysrcSet("ifconfig_"+s.Iface, "mtu "+strconv.Itoa(s.MTU)); err != nil {
			return err
		}
	}
	return nil
}

// persistRcConfStatic6 needs the IPv4 list too: rc.conf numbers alias slots
// across both families, so where the v6 aliases start depends on how many v4
// addresses came before them.
func persistRcConfStatic6(s Spec, v4, v6 []netipPrefix) error {
	for i, p := range v6 {
		key := "ifconfig_" + s.Iface + "_ipv6"
		if i > 0 {
			key = fmt.Sprintf("ifconfig_%s_alias%d", s.Iface, len(v4)+i-1)
		}
		if err := sysrcSet(key, "inet6 "+p.String()); err != nil {
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
