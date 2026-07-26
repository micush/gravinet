package config

import (
	"encoding/json"
	"runtime"
	"testing"
)

func TestWorkerThreadsDefaultAndCap(t *testing.T) {
	var zero Config
	got := zero.WorkerThreadsValue()
	want := DefaultWorkerThreads
	if cpus := runtime.NumCPU(); want > cpus {
		want = cpus
	}
	if got != want {
		t.Fatalf("default worker threads = %d, want %d (DefaultWorkerThreads=%d capped at NumCPU=%d)",
			got, want, DefaultWorkerThreads, runtime.NumCPU())
	}
	// Never zero: tunLoop divides work by this and tunLoopSerial is selected
	// at exactly 1, so 0 would be a live bug rather than a tuning choice.
	if got < 1 {
		t.Fatalf("resolved worker threads %d is below 1", got)
	}
	// An explicit value is honoured as-is, including above NumCPU — the cap
	// applies to the default, not to a deliberate operator choice.
	for _, n := range []int{1, 2, 64} {
		c := Config{WorkerThreads: n}
		if c.WorkerThreadsValue() != n {
			t.Fatalf("explicit worker_threads=%d resolved to %d", n, c.WorkerThreadsValue())
		}
	}
}

func TestTunQueuesDefaultAndExplicitOff(t *testing.T) {
	var zero Config
	got := zero.TunQueuesValue()
	want := DefaultTunQueues
	if cpus := runtime.NumCPU(); want > cpus {
		want = cpus
	}
	if got != want {
		t.Fatalf("default tun queues = %d, want %d", got, want)
	}
	// tun_queues=1 must remain the way to force the old single-queue path now
	// that 0 means "use the default" rather than "off".
	c := Config{TunQueues: 1}
	if c.TunQueuesValue() != 1 {
		t.Fatalf("tun_queues=1 resolved to %d, want 1 (single-queue escape hatch)", c.TunQueuesValue())
	}
}

// udp_gso is a *bool specifically so "absent" and "explicitly false" stay
// distinguishable once the default flipped to on. A plain bool would have
// silently re-enabled it for every operator who had deliberately turned it off.
func TestUDPGSODefaultsOnButRespectsExplicitFalse(t *testing.T) {
	var zero Config
	if !zero.UDPGSOEnabled() {
		t.Fatal("udp_gso should default to enabled when unset")
	}
	off := false
	if (&Config{EnableUDPGSO: &off}).UDPGSOEnabled() {
		t.Fatal("explicit udp_gso=false was ignored")
	}
	on := true
	if !(&Config{EnableUDPGSO: &on}).UDPGSOEnabled() {
		t.Fatal("explicit udp_gso=true was ignored")
	}
}

// The distinction has to survive a round trip through the config file, which
// is where it actually matters.
func TestUDPGSOFalseSurvivesJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"absent", `{}`, true},
		{"explicit false", `{"udp_gso":false}`, false},
		{"explicit true", `{"udp_gso":true}`, true},
	} {
		var c Config
		if err := json.Unmarshal([]byte(tc.body), &c); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := c.UDPGSOEnabled(); got != tc.want {
			t.Fatalf("%s: UDPGSOEnabled()=%v, want %v", tc.name, got, tc.want)
		}
	}
	// And a node that has GSO off must not have that silently dropped when
	// its config is written back out.
	off := false
	b, err := json.Marshal(Config{EnableUDPGSO: &off})
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.UDPGSOEnabled() {
		t.Fatalf("udp_gso=false did not survive marshal/unmarshal: %s", b)
	}
}
