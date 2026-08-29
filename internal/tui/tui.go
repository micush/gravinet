// Package tui is gravinet's terminal console: the web admin's own layout —
// top bar, collapsible left rail, card-based content pane — drawn with
// characters instead of DOM nodes, reached as "gravinet tui".
//
// # Why this exists next to the CLI rather than instead of it
//
// The CLI answers a question and exits. That is the right shape for a script
// and the wrong shape for the ten minutes after something breaks, where the
// questions are "what does this node think its peers are", "is the firewall
// what I left it as", "what is the log saying right now" — asked in that
// order, each one informed by the last, over ssh, on a box with no browser
// reachable from wherever the operator is sitting. Answering those through
// the CLI means knowing the command names in advance; the rail exists
// precisely because that knowledge is what a new operator does not have.
//
// So the TUI is not a third source of truth. Every page here reads through
// the same paths cmd/gravinet's own leaves read through — internal/config for
// the file, internal/control for live daemon state, and the exported host
// readers (webadmin.TakeHostSnapshot, webadmin.LocalRouteTableText,
// webadmin.RunVtysh, service.LLDPNeighbors, resolver.Dump) that the web
// handlers and the CLI already share. There is one implementation of each
// read, reached three ways now instead of two.
//
// # Editing
//
// Mesh (Networks, Keys, Seeds, Peers, Bans) can be edited from here: add,
// edit, delete, and toggle, on the rows this rail already lists. Every other
// group is still read-only, for the reason given below — and that's the
// order this grew in, not a permanent split; the same treatment is meant to
// reach Traffic, Naming, System, and Settings next.
//
// A mutation reaches disk or the daemon one of two ways, and which one is
// never this package's guess. Wherever cmd/gravinet already has a leaf for
// it, an edit here builds the exact argv a person would type and runs the
// real gravinet binary as a subprocess (mutate.go) — the same validation,
// the same persistence, the same audit trail, because it is that command,
// not a second implementation of what it does. A handful of fields exist as
// validated config.Config setters with no CLI verb yet (this node's own
// address on a network, its relay/self-seed/mesh-mode settings, a peer's
// enabled state) — those call the setter directly and run this package's own
// save-and-reload, mirroring cmd/gravinet's commitCfg rather than the
// validation inside it, which still belongs entirely to the setter. Every
// such case says so in its own comment (actions_mesh.go), so a CLI verb
// added for one later is a visible invitation to delete the workaround.
//
// What stays out even where editing has landed: nothing here ever assembles
// a firewall rule, a NAT mapping, or any other structure by hand and writes
// it — every field collected in a form is handed to the one function that
// already knows what a valid value is, and this package's own role stops at
// collecting the fields and reporting what came back.
//
// # What it deliberately does not do
//
// It does not manage other nodes. The web admin's peer picker proxies
// every request through /api/proxy to a Manager-mode peer; that is an
// authenticated HTTP path, and this process has neither a session nor a
// reason to invent one. The TUI shows the node it is running on. "gravinet
// tui" over ssh to that node is the equivalent, and it is one hop.
//
// It does not build a second editor for the groups it hasn't reached yet.
// The web admin's editors are internal/webadmin/edit.go, which is a hundred
// and ten thousand bytes of forms and the validation behind them, and a form
// is not the part of an editor that is hard — the checks it runs before it
// lets you save are. Reproducing those as a second implementation in a
// terminal is how two implementations of the same rule come to disagree, and
// the disagreement would be discovered by a node that took a config the
// other half would have refused — which is exactly why the editing that does
// exist here (above) never reimplements a rule, only calls it. This is the
// same split cmd/gravinet already draws for its own read-only leaves and
// says so in their comments (cmdSystemDHCP, cmdSystemVLANs, cmdIPv6RA): read
// and inspect here until the change is wired through to something that
// already implements it once. Every page with no editor yet names the
// command that reaches it, in a footer line, so the answer to "how do I
// change this" is on the page asking the question rather than in a manual.
//
// # Structure
//
//	term_*.go    raw mode and window size, one file per platform family
//	screen.go    the cell buffer and its diffing writer
//	style.go     the palette, transcribed from ui.go's CSS variables
//	keys.go      byte stream -> key events
//	nav.go       the rail: NAV_GROUPS mirrored, with a test that reads ui.go
//	content.go   cards, tables, key/value rows -> styled lines
//	rows.go      selectable-row identity and locating a row's on-screen line
//	sections.go  one builder per page (sections_monitor.go: the rest)
//	data.go      the snapshot every page reads from
//	app.go       the model: layout, event handling, polling, row selection
//	search.go    the top bar's search box
//	mutate.go    how an edit reaches disk or the daemon — see "Editing" above
//	forms.go     the modal form/confirm/result overlays an edit uses
//	actions.go   the per-section add/edit/delete/toggle dispatch table
//	actions_*.go one file per rail group's actual editing actions
//
// The model in app.go never touches a file descriptor: it takes key events
// and a snapshot and draws into a Screen, which is an addressable buffer with
// a String method. That is what makes the whole thing testable without a
// terminal, and nearly every test in this package drives it that way.
package tui

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Options configures a TUI session. The zero value is usable: every field
// falls back to the same default the CLI uses for it.
type Options struct {
	// ConfigPath is the config.json to read. Empty means the platform
	// default, resolved by the caller (cmd/gravinet knows it; this package
	// deliberately does not hardcode a second copy of that logic).
	ConfigPath string

	// ControlSocket is the daemon's control socket. Empty means the path in
	// the loaded config, falling back to the caller's default.
	ControlSocket string

	// Theme selects the palette: "dark" (the web admin's default), "light",
	// or "" to follow the terminal's own hint (see detectTheme).
	Theme string

	// Color forces a color depth: "truecolor", "256", "mono", or "" to
	// detect. Detection is right nearly always; the override exists because
	// "nearly" is doing real work in terminals that lie about $TERM.
	Color string

	// Interval is how often live pages re-read their data. Zero selects
	// defaultInterval.
	Interval time.Duration

	// Version and Commit are the running binary's build identity, passed in
	// rather than read here: they are ldflags-set variables in package main,
	// and a copy in this package would be a second version number that could
	// disagree with what "gravinet version" prints.
	Version, Commit string

	// In and Out are the terminal. Both nil means os.Stdin/os.Stdout.
	// Set in tests to drive a session without a tty.
	In  *os.File
	Out *os.File
}

// defaultInterval matches the web admin's own status poll (startPolling in
// ui.go). Live pages are cheap — a control-socket round trip on a unix
// socket, or one local file read — but "cheap" is not "free" on a box that is
// already in trouble, which is when this is most likely to be open.
const defaultInterval = 3 * time.Second

// ErrNotATerminal is returned when stdin or stdout is not a terminal. The TUI
// needs both: one to put in raw mode, one to size and draw into. Returned
// rather than worked around, because the useful thing to say to somebody who
// piped this into a file is that the CLI is what they wanted.
var ErrNotATerminal = errors.New("not a terminal")

// Run opens the console and blocks until the operator quits. The terminal is
// restored on every exit path, including a panic: a TUI that leaves a shell
// in raw mode with no echo has done more damage than the error it failed on.
func Run(opts Options) error {
	in, out := opts.In, opts.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if !isTerminal(in) || !isTerminal(out) {
		return fmt.Errorf("%w: gravinet tui needs an interactive terminal on both stdin and stdout"+
			" (for a pipe or a script, the same pages are available as \"gravinet <group> <section>\")", ErrNotATerminal)
	}

	restore, err := enterRaw(in)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	// Ordered so the screen is put back before the terminal modes are: the
	// alt-screen pop is itself written through the terminal this is about to
	// stop configuring, and doing it the other way round leaves the sequence
	// racing the restore on some emulators.
	defer func() {
		leaveAltScreen(out)
		restore()
	}()

	if err := enterAltScreen(out); err != nil {
		return err
	}

	app := newApp(opts, out)
	return app.run(in)
}
