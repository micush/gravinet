package main

import (
	"os"
	"strings"
	"testing"
)

// hostnet asserts per-interface IPv6 forwarding on interfaces it addresses,
// and it has to be told the daemon's ip_forwarding setting to do it on the
// right terms. The wiring is one line in a startup path with no return value
// and no observable effect from a test, so it is pinned here rather than left
// to be silently deleted.
//
// What matters is that the call is unconditional. Putting it inside the
// `if cfg.ForwardingEnabled()` block would leave the opt-out case never
// telling hostnet anything, and hostnet's default is on — so opting out would
// have turned the per-interface knob on anyway, which is the exact failure
// the setting exists to prevent.
func TestForwarding6IsWiredToTheConfigSetting(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	const call = "hostnet.SetForwarding6(cfg.ForwardingEnabled())"
	i := strings.Index(src, call)
	if i < 0 {
		t.Fatal("the daemon no longer tells hostnet whether it may enable IPv6 forwarding")
	}
	if strings.Count(src, "hostnet.SetForwarding6(") != 1 {
		t.Error("more than one place sets this; the last one to run silently wins")
	}

	// The gate opens on the line after, so the call must sit above it.
	g := strings.Index(src, "if cfg.ForwardingEnabled() {")
	if g < 0 {
		t.Fatal("the ipfwd gate has moved; check this wiring is still outside it")
	}
	if i > g {
		t.Error("SetForwarding6 is inside the ForwardingEnabled gate, so opting out never reaches hostnet")
	}
}
