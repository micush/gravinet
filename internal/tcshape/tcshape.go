// Package tcshape programs kernel-side traffic shaping on interfaces gravinet
// does not carry packets for.
//
// This is the part the userspace shaper cannot do. internal/mesh shapes by
// owning the data path: overlay packets go into a bounded queue and a drainer
// paces them out of a token bucket, which works because every one of those
// packets passes through gravinet on its way to a TUN device gravinet created.
// Traffic on a physical NIC never does. There is no gravinet code between the
// application and the wire to delay anything, so the only thing that can pace
// it is the kernel's own queueing discipline.
//
// So the two mechanisms split by interface, not by preference:
//
//   - A mesh interface is shaped in userspace (internal/mesh). That path knows
//     about QoS classes and exempts control traffic, neither of which a qdisc
//     can see, so it stays where it is.
//   - Any other interface is shaped here, by programming tc.
//
// One config surface (config.Shaping) drives both; config.ShapingKind says
// which applies to a given entry.
//
// # What this takes over
//
// Unlike internal/netfilter, which owns a private nft table or iptables chain
// and can therefore promise it never touches the operator's own rules, tc has
// no namespace to hide in: an interface has one root qdisc, and shaping it
// means being that qdisc. Programming an interface here **replaces whatever
// root qdisc it had**.
//
// That is why nothing is implicit. An interface is only ever programmed when
// the operator has added a shaping entry naming it, Manager remembers exactly
// which interfaces it programmed, and Clear touches only those. An interface
// nobody asked about is never opened.
//
// The plan generators below are pure and platform-neutral so they can be unit
// tested without root or tc installed; only execution lives in the platform
// files.
package tcshape

import (
	"fmt"
	"strings"
)

// Iface is the shaping requested for one interface. Rates are bytes per
// second; 0 means unlimited in that direction, and an Iface unlimited in both
// is a request to remove shaping rather than to install any.
type Iface struct {
	Name            string
	UpBytesPerSec   int // egress, shaped (tbf paces it)
	DownBytesPerSec int // ingress, policed (over-rate is dropped)
	// BurstBytes is the token-bucket depth. 0 takes the default from
	// BurstFor, which is what the admin UI and CLI always leave it as.
	BurstBytes int
}

// Shaped reports whether this entry asks for anything at all.
func (i Iface) Shaped() bool { return i.UpBytesPerSec > 0 || i.DownBytesPerSec > 0 }

// Cmd is one tc invocation. Tolerant marks the teardown steps that may
// legitimately fail — deleting a qdisc that was never installed is the normal
// case on a first apply, not an error worth surfacing.
type Cmd struct {
	Args     []string
	Tolerant bool
}

// ingressHandle is the conventional handle for an ingress qdisc; tc requires
// exactly ffff: here, so it is not a choice.
const ingressHandle = "ffff:"

// minBurstBytes floors the token bucket at roughly one maximum-size frame.
//
// A bucket smaller than a single packet cannot pass that packet at all, and
// one only slightly larger shreds TCP: every ordinary micro-burst overflows
// it and the sender collapses well below the configured rate. Same reasoning,
// and same failure, as the userspace policer's burst floor.
const minBurstBytes = 1600

// queueLatency is how long the egress bucket is willing to hold a packet
// before dropping it, which is what turns a rate into a queue depth. 100ms is
// the usual compromise: long enough that a burst is smoothed rather than
// discarded, short enough that a saturated link does not accumulate the
// multi-second standing queue that makes an otherwise-working link feel dead.
const queueLatency = "100ms"

// BurstFor is the token-bucket depth for a rate: a quarter second of it, never
// below one frame. Explicit BurstBytes from config wins.
func BurstFor(bytesPerSec, explicit int) int {
	if explicit > 0 {
		return explicit
	}
	b := bytesPerSec / 4
	if b < minBurstBytes {
		b = minBurstBytes
	}
	return b
}

// bitRate renders a bytes/sec rate the way tc wants it. Deliberately in bits:
// tc's own "bps" suffix means *bytes* per second, which reads as bits to
// almost everyone, so spelling the unit out avoids a factor-of-eight error in
// whichever direction the reader guesses.
func bitRate(bytesPerSec int) string { return fmt.Sprintf("%dbit", bytesPerSec*8) }

// Plan returns the tc invocations that bring one interface to the requested
// state, including removing a direction that is no longer limited.
//
// Egress uses tbf, which delays rather than drops — we are the sender, so
// pacing is available and is strictly better than discarding our own traffic.
// Ingress uses a policer, which drops, because the sender is on the other end
// of the link and there is nothing local to slow down; for TCP the drop is
// itself the signal to back off.
func Plan(i Iface) []Cmd {
	var out []Cmd
	if i.Name == "" {
		return nil
	}

	// Egress. "replace" rather than add, so re-applying an unchanged config
	// is a no-op in effect and a changed rate does not need a delete first.
	if i.UpBytesPerSec > 0 {
		out = append(out, Cmd{Args: []string{
			"qdisc", "replace", "dev", i.Name, "root", "tbf",
			"rate", bitRate(i.UpBytesPerSec),
			"burst", fmt.Sprintf("%d", BurstFor(i.UpBytesPerSec, i.BurstBytes)),
			"latency", queueLatency,
		}})
	} else {
		out = append(out, Cmd{Tolerant: true, Args: []string{"qdisc", "del", "dev", i.Name, "root"}})
	}

	// Ingress. The qdisc is torn down and recreated rather than replaced,
	// because the policer lives in a filter attached to it and filters
	// accumulate: "replace" on the qdisc would leave the previous rate's
	// filter in place alongside the new one, and the lower of the two would
	// silently win.
	out = append(out, Cmd{Tolerant: true, Args: []string{"qdisc", "del", "dev", i.Name, "ingress"}})
	if i.DownBytesPerSec > 0 {
		out = append(out, Cmd{Args: []string{"qdisc", "add", "dev", i.Name, "handle", ingressHandle, "ingress"}})
		// One filter per address family, rather than one "protocol all".
		// Families are kept explicit here the same way internal/netfilter
		// keeps them explicit, and a v6-only host is not left unpoliced by an
		// idiom that quietly only matched v4.
		for prio, proto := range []string{"ip", "ipv6"} {
			out = append(out, Cmd{Args: []string{
				"filter", "add", "dev", i.Name, "parent", ingressHandle,
				"protocol", proto, "prio", fmt.Sprintf("%d", prio+1),
				// u32 with an always-true match, not matchall. matchall is
				// the tidier spelling but needs cls_matchall (Linux 4.10+),
				// and it is genuinely absent on kernels that have u32: the
				// container this was developed in rejected matchall outright
				// ("TC classifier not found") while accepting u32. Since the
				// two express the same thing here — match every packet — the
				// one with the wider reach wins.
				"u32", "match", "u32", "0", "0",
				"action", "police",
				"rate", bitRate(i.DownBytesPerSec),
				"burst", fmt.Sprintf("%d", BurstFor(i.DownBytesPerSec, i.BurstBytes)),
				"conform-exceed", "drop",
			}})
		}
	}
	return out
}

// ClearPlan removes everything Plan may have installed on an interface. Every
// step is tolerant: this runs on shutdown and when an entry is deleted, and by
// then the interface may be down, renamed or gone.
func ClearPlan(name string) []Cmd {
	if name == "" {
		return nil
	}
	return []Cmd{
		{Tolerant: true, Args: []string{"qdisc", "del", "dev", name, "root"}},
		{Tolerant: true, Args: []string{"qdisc", "del", "dev", name, "ingress"}},
	}
}

// String renders a Cmd for logs.
func (c Cmd) String() string { return "tc " + strings.Join(c.Args, " ") }
