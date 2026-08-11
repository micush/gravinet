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
