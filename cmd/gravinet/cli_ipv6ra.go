package main

import (
	"fmt"

	"gravinet/internal/config"
)

// cmdIPv6RA is the CLI leaf for Traffic > IPv6 Router Advertisements. It
// exists for nav parity — every web-admin page has a CLI counterpart, and a
// page reachable only in a browser is a page an operator cannot script or
// inspect over ssh.
//
// Read and state-toggle only, deliberately. Adding an interface needs a
// prefix, DNS and search list, which the editor handles with validation and
// an interface picker; reproducing that as flags would be a second, weaker
// implementation of the same form. Listing and parking are the operations
// worth having on a terminal at 3am.
func cmdIPv6RA(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub := expandVerb(args[0], v("list"), v("enable", "disable"))
	netName, rest := extractOpt(args[1:], "net")
	_ = netName
	cfg, path, rest := openCfg(rest)
	ra := &cfg.RouterAdvert

	switch sub {
	case "list":
		fmt.Printf("router advertisements: %s\n", onOff(ra.Enabled))
		if len(ra.Interfaces) == 0 {
			fmt.Println("  (none)")
			return
		}
		for _, e := range ra.Interfaces {
			pfx := "(interface's own /64s)"
			if len(e.Prefixes) > 0 {
				pfx = joinComma(e.Prefixes)
			}
			fmt.Printf("  %-10s %-8s %-28s pref=%s dns=%s search=%s\n",
				e.Iface, onOff(!e.Disabled), pfx,
				orDash(e.Preference), orDash(joinComma(e.DNS)), orDash(joinComma(e.Search)))
		}

	case "enable", "disable":
		if len(rest) == 0 {
			fatal("usage: gravinet traffic ipv6ra %s IFACE", sub)
		}
		found := false
		for i := range ra.Interfaces {
			if ra.Interfaces[i].Iface == rest[0] {
				ra.Interfaces[i].Disabled = sub == "disable"
				found = true
			}
		}
		if !found {
			fatal("no advertisement configured for interface %q", rest[0])
		}
		if err := cfg.Validate(); err != nil {
			fatal("invalid config after change: %v", err)
		}
		if err := cfg.SaveTo(path); err != nil {
			fatal("save config: %v", err)
		}
		fmt.Printf("%sd router advertisements on %s\n", sub, rest[0])
		if reloadDaemon(cfg.ControlSocket) {
			fmt.Println("daemon reloaded")
		}
		fmt.Println("note: radvd.conf is rewritten by the web admin's apply; use it to add or edit an interface")

	default:
		fatal("usage: gravinet traffic ipv6ra <list|enable|disable> [IFACE]")
	}
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ = config.RAInterface{}
