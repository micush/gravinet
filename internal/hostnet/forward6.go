package hostnet

import "sync/atomic"

// forward6 is whether this package may turn IPv6 forwarding on for an
// interface it has just addressed.
//
// It exists because `ip_forwarding: false` is a real setting and means what it
// says. gravinet's whole forwarding story is host-level — ipfwd flips the two
// global knobs at startup and puts them back on shutdown — so an operator who
// opted out and then edited an interface's address would otherwise find the
// per-interface knob turned on underneath them by a code path that never
// mentions forwarding. That is the sort of thing that gets discovered months
// later, on the machine where it mattered.
//
// Process-wide rather than a Spec field because that is what the setting is:
// one host decision, made once, that both callers of Apply are subject to.
// Atomic because those callers are the reload path and an HTTP handler, on
// different goroutines.
//
// Defaults on, matching config.ForwardingEnabled's own default for an unset
// field, so the two cannot disagree about what "not configured" means.
var forward6 atomic.Bool

func init() { forward6.Store(true) }

// SetForwarding6 records whether interfaces gravinet addresses may have IPv6
// forwarding turned on. Called by the daemon at startup from the same
// ip_forwarding setting that drives ipfwd, so this package and the global
// knobs are never enabled on different terms.
//
// Not re-read on reload, which matches ipfwd: the global knobs are set once at
// startup too, and having the per-interface assertion follow a mid-life config
// change while the host-wide one did not would be worse than either.
func SetForwarding6(on bool) { forward6.Store(on) }
