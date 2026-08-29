package tui

// The host interface list, read through net.Interfaces exactly as
// cmd/gravinet's cmdSystemInterfaces reads it. Split into its own file only
// so data.go stays a description of where each page's data comes from rather
// than an implementation of one of them.

import "net"

func netInterfaces() ([]ifaceRow, error) {
	ifis, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceRow, 0, len(ifis))
	for _, ifi := range ifis {
		// The same three-state reading cmdSystemInterfaces uses: "up" alone
		// is not the whole story, because an administratively-up interface
		// with no carrier is the exact condition somebody is looking for when
		// they open this page, and it reads as "up" if you only check
		// FlagUp.
		st := "down"
		if ifi.Flags&net.FlagUp != 0 {
			st = "up"
			if ifi.Flags&net.FlagRunning == 0 {
				st = "up, no carrier"
			}
		}
		row := ifaceRow{name: ifi.Name, state: st, mtu: ifi.MTU, mac: ifi.HardwareAddr.String()}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			row.addrs = append(row.addrs, a.String())
		}
		out = append(out, row)
	}
	return out, nil
}
