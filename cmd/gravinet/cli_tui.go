package main

// "gravinet tui" — the terminal console. See internal/tui's package comment
// for what it is and, more usefully, for what it deliberately is not.
//
// This file is thin on purpose. Everything about the console lives in
// internal/tui; what belongs here is the two things package main knows and
// that package should not: where this platform's config lives (the same
// resolution every other CLI command uses, including the GRAVINET_CONFIG
// override) and what version this binary is, which is an ldflags-set variable
// in this package. Both are passed in rather than re-derived, so the console's
// About page and "gravinet version" cannot disagree.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"gravinet/internal/tui"
)

func cmdTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config path (default: this platform's, or $GRAVINET_CONFIG)")
	sock := fs.String("sock", "", "control socket path (default: the one in the config)")
	theme := fs.String("theme", "", "palette: dark|light (default: detected from the terminal)")
	color := fs.String("color", "", "color depth: truecolor|256|mono (default: detected)")
	interval := fs.Duration("interval", 0, "how often live pages re-read (default 3s)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `gravinet tui — the web admin's layout, in a terminal.

The same groups and the same pages as the left rail in a browser, reading the
same config file and the same control socket the rest of the CLI reads. Live
pages (peers, bans, routes, metrics, logs) refresh on a timer; r re-reads
everything, / searches every page, ? lists the keys, q quits.

Mesh, Traffic, Naming, System, and Settings can be edited from here. Two
patterns, both shown at the bottom of the page they apply to:

  - Lists (networks, firewall rules, users, BGP neighbors, and the like):
    a adds, e edits, d deletes, space toggles enabled, on whichever row the
    cursor is on.
  - Named settings (Settings itself, a hostname, an enabled flag): one
    character in the field's own label is underlined — press it, from
    anywhere on the page, to edit that field immediately.

Every mutation runs through the same gravinet leaf a person would type, or
the same validated config setter the web admin uses, so there is one
implementation of what a valid change is, never a second one here that
could disagree with it. Monitor and Info are read-only by nature (there is
nothing to edit); a handful of individual fields elsewhere stay read-only
for a stated reason (e.g. IPv6RA's full interface entries and DHCP's relay
links need validation only the web admin's form does, and "upgrade apply"
runs a multi-minute build this console's short-lived mutations aren't built
for) — each of those says so on its own page rather than pretending
otherwise.

flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	path := *cfgPath
	if path == "" {
		// The same resolution defaultControlSocket does, and for the same
		// reason: a node that moved its config said so in the environment,
		// and a console that ignored that would be reading a different file
		// from the daemon it is reporting on.
		path = os.Getenv("GRAVINET_CONFIG")
		if path == "" {
			path = defaultConfigPath
		}
	}
	sockPath := *sock
	if sockPath == "" {
		sockPath = defaultControlSocket()
	}

	err := tui.Run(tui.Options{
		ConfigPath:    path,
		ControlSocket: sockPath,
		Theme:         *theme,
		Color:         *color,
		Interval:      time.Duration(*interval),
		Version:       version,
		Commit:        commit,
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, tui.ErrNotATerminal):
		// Not a failure worth a stack of context: somebody piped this, and
		// the useful reply is which command they wanted instead. Run's own
		// message says that, so print it and stop.
		fatal("%v", err)
	default:
		fatal("tui: %v", err)
	}
}
