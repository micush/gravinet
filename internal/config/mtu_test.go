package config

import (
	"strings"
	"testing"

	"gravinet/internal/protocol"
)

func mtuTestCfg() *Config {
	return &Config{Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24", MTU: 9216}}}
}

// The value that motivated the command existing at all: setting the MTU to
// what actually fits must succeed and must NOT come with an advisory.
func TestNetworkSetMTUAcceptsAFittingValue(t *testing.T) {
	c := mtuTestCfg()
	advice, err := c.NetworkSetMTU("lan", protocol.DefaultTunnelMTU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if advice != "" {
		t.Fatalf("unexpected advisory for a fitting MTU: %s", advice)
	}
	if got := c.FindNetwork("lan").MTU; got != protocol.DefaultTunnelMTU {
		t.Fatalf("MTU is %d, want %d", got, protocol.DefaultTunnelMTU)
	}
}

// An MTU larger than any path could carry whole is allowed — an operator may
// have a reason — but it must say so, and the number it names must be one that
// actually resolves the condition.
func TestNetworkSetMTUAdvisesWhenItWillFragmentEverything(t *testing.T) {
	c := mtuTestCfg()
	advice, err := c.NetworkSetMTU("lan", 9216)
	if err != nil {
		t.Fatalf("9216 should be accepted, not rejected: %v", err)
	}
	if advice == "" {
		t.Fatal("no advisory for an MTU that fragments every packet")
	}
	fits := MaxUnfragmentedTunnelMTU(c.UnderlayMTUMaxValue())
	for _, want := range []string{"9216", "fragment"} {
		if !strings.Contains(advice, want) {
			t.Fatalf("advisory missing %q: %s", want, advice)
		}
	}
	// The advisory has to name a value that genuinely fixes it — the exact
	// failure of the v658 warning this replaced.
	if !strings.Contains(advice, itoa(fits)) {
		t.Fatalf("advisory does not name the fitting MTU %d: %s", fits, advice)
	}
	if a2, err := c.NetworkSetMTU("lan", fits); err != nil || a2 != "" {
		t.Fatalf("the MTU the advisory recommends still advises (%q) or errored (%v)", a2, err)
	}
}

func TestNetworkSetMTURejectsOutOfRangeAndUnknownNetwork(t *testing.T) {
	c := mtuTestCfg()
	for _, bad := range []int{0, -1, protocol.MinTunnelMTU - 1, 65536} {
		if _, err := c.NetworkSetMTU("lan", bad); err == nil {
			t.Fatalf("mtu %d was accepted", bad)
		}
	}
	// A rejected value must not have been written.
	if got := c.FindNetwork("lan").MTU; got != 9216 {
		t.Fatalf("a rejected edit changed the MTU to %d", got)
	}
	if _, err := c.NetworkSetMTU("nope", 8915); err == nil {
		t.Fatal("unknown network was accepted")
	}
}

// MaxUnfragmentedTunnelMTU duplicates mesh.computeMaxInnerFrag (config cannot
// import mesh); mesh pins the two against each other. This just checks the
// arithmetic here is the arithmetic documented on protocol.DefaultTunnelMTU.
func TestMaxUnfragmentedTunnelMTUMatchesDefault(t *testing.T) {
	if got := MaxUnfragmentedTunnelMTU(9000); got != protocol.DefaultTunnelMTU {
		t.Fatalf("MaxUnfragmentedTunnelMTU(9000)=%d, want %d", got, protocol.DefaultTunnelMTU)
	}
	if got := MaxUnfragmentedTunnelMTU(10); got != 1 {
		t.Fatalf("tiny ceiling should floor at 1, got %d", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
