package main

import (
	"fmt"
	"net"
	"sort"
)

// cmdSystemInterfaces prints this host's interfaces and addresses — the CLI
// counterpart to System > Interfaces, and read-only for the same reasons (see
// internal/webadmin/sysifaces.go).
//
// Read from the host directly rather than through the daemon: an interface
// list is exactly what an operator wants when the daemon is the thing that
// will not start.
func cmdSystemInterfaces(args []string) {
	ifis, err := net.Interfaces()
	if err != nil {
		fatal("read interfaces: %v", err)
	}
	sort.Slice(ifis, func(i, j int) bool { return ifis[i].Name < ifis[j].Name })
	for _, ifi := range ifis {
		st := "down"
		if ifi.Flags&net.FlagUp != 0 {
			st = "up"
			if ifi.Flags&net.FlagRunning == 0 {
				st = "up, no carrier"
			}
		}
		fmt.Printf("%-12s %-14s mtu %-6d %s\n", ifi.Name, st, ifi.MTU, ifi.HardwareAddr)
		addrs, _ := ifi.Addrs()
		if len(addrs) == 0 {
			fmt.Println("             (no addresses)")
			continue
		}
		for _, a := range addrs {
			fmt.Printf("             %s\n", a.String())
		}
	}
}

// cmdSystemVLANs lists the tagged interfaces gravinet is configured to create,
// alongside what the host actually has.
//
// Read-only, the same split cmdSystemDHCP and cmdIPv6RA draw. Creating one
// needs a parent that exists, a tag in range, and a name that does not collide
// with either — a form that already checks all three, and reproducing it as
// flags would be a second, weaker copy of it. What is worth having on a
// terminal is seeing whether the device the config promises is actually there,
// which is the question the daemon's own log answers only at startup.
func cmdSystemVLANs(args []string) {
	cfg, _, _ := openCfg(args)
	if len(cfg.HostVLANs) == 0 {
		fmt.Println("no tagged interfaces configured")
		return
	}
	for _, v := range cfg.HostVLANs {
		name := v.VLANName()
		state := "enabled"
		if v.Disabled {
			state = "disabled"
		}
		present := "missing"
		if ifi, err := net.InterfaceByName(name); err == nil {
			present = "down"
			if ifi.Flags&net.FlagUp != 0 {
				present = "up"
			}
		}
		fmt.Printf("%-15s %-8s vlan %-5d on %-12s %s\n", name, state, v.ID, v.Parent, present)
	}
	fmt.Println("note: tagged interfaces are created and removed through the web admin's System > Interfaces page")
}
