package tcshape

import (
	"strings"
	"testing"
)

func joined(cmds []Cmd) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.String())
	}
	return out
}

func hasCmd(cmds []Cmd, substr string) bool {
	for _, c := range cmds {
		if strings.Contains(c.String(), substr) {
			return true
		}
	}
	return false
}

// Rates go to tc in bits, spelled "bit". tc's own "bps" suffix means *bytes*
// per second, which reads as bits to almost everyone — getting this wrong is a
// silent factor-of-eight error in whichever direction the reader guessed, and
// a link capped at an eighth of its intended rate looks like a broken network
// rather than a unit bug.
func TestRatesAreRenderedInBits(t *testing.T) {
	p := Plan(Iface{Name: "eth0", UpBytesPerSec: 1_250_000})
	if !hasCmd(p, "rate 10000000bit") {
		t.Errorf("1250000 bytes/s should render as 10000000bit, got:\n%s", strings.Join(joined(p), "\n"))
	}
}

// Egress is shaped and ingress is policed, and the difference is not cosmetic:
// we are the sender on egress so a packet can be delayed, and we are not on
// ingress so the only lever is to drop. Swapping them would either drop
// traffic we could have paced, or try to pace a sender we do not control.
func TestEgressShapesAndIngressPolices(t *testing.T) {
	p := Plan(Iface{Name: "eth0", UpBytesPerSec: 1_000_000, DownBytesPerSec: 2_000_000})
	if !hasCmd(p, "root tbf") {
		t.Error("egress is not a tbf, so outbound traffic is dropped rather than paced")
	}
	if !hasCmd(p, "action police") || !hasCmd(p, "conform-exceed drop") {
		t.Error("ingress is not policed, so there is nothing to enforce the down-rate")
	}
	// u32, not matchall: matchall needs cls_matchall (4.10+) and is absent on
	// kernels that still carry u32, so it would silently leave ingress
	// unpoliced on exactly the older hosts most likely to need a cap.
	if !hasCmd(p, "u32 match u32 0 0") {
		t.Error("the ingress classifier is not u32, so policing will fail on kernels without cls_matchall")
	}
	// Both families, or a v6-only host is policed by nothing.
	if !hasCmd(p, "protocol ip prio 1") || !hasCmd(p, "protocol ipv6 prio 2") {
		t.Errorf("ingress does not police both address families:\n%s", strings.Join(joined(p), "\n"))
	}
	if !hasCmd(p, "ingress") {
		t.Error("no ingress qdisc, so the policing filter has nothing to attach to")
	}
}

// A direction set to unlimited must actively remove what a previous apply
// installed. Leaving it in place would mean clearing a cap in the UI and
// having it keep applying, which is the silent-success failure this whole
// surface exists to avoid.
func TestUnlimitedDirectionIsTornDown(t *testing.T) {
	p := Plan(Iface{Name: "eth0", DownBytesPerSec: 2_000_000}) // up unlimited
	if !hasCmd(p, "qdisc del dev eth0 root") {
		t.Error("an unlimited up-rate leaves a stale root qdisc pacing egress")
	}
	if hasCmd(p, "root tbf") {
		t.Error("an unlimited up-rate installed a shaper anyway")
	}

	p2 := Plan(Iface{Name: "eth0", UpBytesPerSec: 1_000_000}) // down unlimited
	if !hasCmd(p2, "qdisc del dev eth0 ingress") {
		t.Error("an unlimited down-rate leaves a stale policer dropping inbound traffic")
	}
	if hasCmd(p2, "action police") {
		t.Error("an unlimited down-rate installed a policer anyway")
	}
}

// The ingress qdisc is deleted before being added, never "replaced". The
// policer lives in a filter attached to it, and filters accumulate: replacing
// the qdisc would leave the old rate's filter beside the new one, and the
// lower of the two would silently win. A rate lowered once and raised back
// would then stay low with nothing on screen explaining it.
func TestIngressIsRebuiltSoFiltersCannotAccumulate(t *testing.T) {
	p := Plan(Iface{Name: "eth0", DownBytesPerSec: 2_000_000})
	del, add := -1, -1
	for i, c := range p {
		s := c.String()
		if strings.Contains(s, "qdisc del dev eth0 ingress") {
			del = i
		}
		if strings.Contains(s, "qdisc add dev eth0 handle ffff: ingress") {
			add = i
		}
	}
	if del < 0 || add < 0 {
		t.Fatalf("expected an ingress delete then add, got:\n%s", strings.Join(joined(p), "\n"))
	}
	if del > add {
		t.Error("the ingress qdisc is added before the old one is removed, so policing filters accumulate")
	}
}

// Teardown of a qdisc that was never installed is the normal case on a first
// apply. If those steps were fatal, the very first apply on a clean interface
// would fail and no rate would ever be programmed.
func TestTeardownStepsAreTolerant(t *testing.T) {
	for _, c := range Plan(Iface{Name: "eth0", UpBytesPerSec: 1_000_000}) {
		if strings.Contains(c.String(), "qdisc del") && !c.Tolerant {
			t.Errorf("%q is fatal, so a first apply on a clean interface fails", c)
		}
	}
	for _, c := range ClearPlan("eth0") {
		if !c.Tolerant {
			t.Errorf("%q is fatal, but teardown runs on shutdown when the interface may be down or gone", c)
		}
	}
}

// ClearPlan has to remove both halves. Leaving the policer behind would keep
// dropping inbound traffic on an interface nobody is shaping any more —
// invisible in the config and attributable to nothing.
func TestClearRemovesBothDirections(t *testing.T) {
	p := ClearPlan("eth0")
	if !hasCmd(p, "qdisc del dev eth0 root") || !hasCmd(p, "qdisc del dev eth0 ingress") {
		t.Errorf("clear does not remove both directions:\n%s", strings.Join(joined(p), "\n"))
	}
}

// The burst floor exists because a bucket around one packet shreds TCP: every
// ordinary micro-burst overflows it and the sender collapses far below the
// configured rate, so the link measures much slower than it was capped at.
func TestBurstIsFlooredAndOverridable(t *testing.T) {
	if got := BurstFor(1000, 0); got != minBurstBytes {
		t.Errorf("burst for a tiny rate = %d, want the %d floor", got, minBurstBytes)
	}
	if got := BurstFor(4_000_000, 0); got != 1_000_000 {
		t.Errorf("burst = %d, want a quarter second (1000000)", got)
	}
	if got := BurstFor(4_000_000, 77); got != 77 {
		t.Errorf("explicit burst = %d, want the configured 77", got)
	}
}

// An entry unlimited in both directions is a request to remove shaping, not to
// install any — Manager.Apply keys off this to decide whether to program an
// interface or unprogram it.
func TestShapedReportsWhetherAnythingWasAsked(t *testing.T) {
	if (Iface{Name: "eth0"}).Shaped() {
		t.Error("an all-unlimited entry claims to ask for shaping")
	}
	if !(Iface{Name: "eth0", DownBytesPerSec: 1}).Shaped() {
		t.Error("an entry with a down-rate claims to ask for nothing")
	}
}

// A nameless entry must produce no commands at all: tc with an empty dev would
// either error or, worse, be interpreted as something else.
func TestNoNameProducesNoCommands(t *testing.T) {
	if got := Plan(Iface{UpBytesPerSec: 1_000_000}); len(got) != 0 {
		t.Errorf("planned %d command(s) for a nameless interface", len(got))
	}
	if got := ClearPlan(""); len(got) != 0 {
		t.Errorf("planned %d teardown command(s) for a nameless interface", len(got))
	}
}
