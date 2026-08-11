//go:build darwin

package hostnet

import (
	"fmt"
	"os/exec"
	"strings"
)

// macOS keeps addressing in SystemConfiguration, not in a file, and
// networksetup(8) is the supported way in. It works on *network services*
// ("Wi-Fi", "Ethernet") rather than BSD interface names, so the device has to
// be mapped back to its service first — an address set against a device name
// silently belongs to nothing.
func detect() *Backend {
	if _, err := exec.LookPath("networksetup"); err != nil {
		return nil
	}
	// networksetup applies and persists in one step, so there is nothing for
	// Reapply to do beyond succeeding — a DHCP lease is already being sought
	// by the time Persist returns.
	return &Backend{
		Name:    "networksetup",
		Persist: persistNetworksetup,
		Reapply: func(Spec) error { return nil },
	}
}

func persistNetworksetup(s Spec) error {
	svc, err := serviceForDevice(s.Iface)
	if err != nil {
		return err
	}
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	v4, v6 := v4v6(s.StaticAddrs(s.Addrs))

	if mode4 == ModeDHCP {
		if out, err := exec.Command("networksetup", "-setdhcp", svc).CombinedOutput(); err != nil {
			return fmt.Errorf("networksetup -setdhcp: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	} else if len(v4) > 0 {
		args := []string{"-setmanual", svc, v4[0].Addr().String(), dottedMask(v4[0])}
		if s.GW4.IsValid() {
			args = append(args, s.GW4.String())
		}
		if out, err := exec.Command("networksetup", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("networksetup -setmanual: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	// macOS is the one platform that does not separate DHCPv6 from SLAAC.
	// -setv6automatic is "configure IPv6 from the network", and whether that
	// ends up being autoconfiguration or DHCPv6 is decided by the flags in the
	// router's advertisement, not here.
	//
	// Both modes map onto it rather than dhcp6 being refused, because unlike
	// on the BSDs it does work: macOS has a DHCPv6 client and will use it when
	// the RA asks for one. What an operator does not get here is the ability to
	// insist on one over the other, which is the platform's limit and not
	// something a wrapper can invent.
	//
	// A static family with no addresses is left alone rather than switched
	// off. -setv6off would be the symmetric thing to do and would break every
	// record written before this release: those carry no mode, which reads as
	// static, and most of them were an operator setting an IPv4 address on a
	// host whose IPv6 was working fine without gravinet's involvement.
	switch {
	case mode6 == ModeSLAAC || mode6 == ModeDHCP6:
		if out, err := exec.Command("networksetup", "-setv6automatic", svc).CombinedOutput(); err != nil {
			return fmt.Errorf("networksetup -setv6automatic: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	case len(v6) > 0:
		args := []string{"-setv6manual", svc, v6[0].Addr().String(), itoa(v6[0].Bits())}
		if s.GW6.IsValid() {
			args = append(args, s.GW6.String())
		}
		if out, err := exec.Command("networksetup", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("networksetup -setv6manual: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if s.MTU > 0 {
		if out, err := exec.Command("networksetup", "-setMTU", svc, itoa(s.MTU)).CombinedOutput(); err != nil {
			return fmt.Errorf("networksetup -setMTU: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	// Only the first address of each family is persisted: networksetup has no
	// form for secondary addresses. Reported rather than dropped silently.
	if len(v4) > 1 || len(v6) > 1 {
		return fmt.Errorf("persisted the first address of each family; macOS networksetup has no way to store secondary addresses")
	}
	return nil
}

// serviceForDevice maps a BSD device name to the network service that owns it.
func serviceForDevice(dev string) (string, error) {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "", fmt.Errorf("networksetup -listallhardwareports: %w", err)
	}
	var port string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Hardware Port:") {
			port = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		}
		if strings.HasPrefix(line, "Device:") &&
			strings.TrimSpace(strings.TrimPrefix(line, "Device:")) == dev {
			return port, nil
		}
	}
	return "", fmt.Errorf("no macOS network service uses device %s, so its addressing cannot be stored", dev)
}
