package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

func TestRouteAdvIntervalConfigurable(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	e := NewEngine(Options{NodeID: "x", RouteAdvInterval: 2 * time.Second,
		Nets: []NetSpec{{ID: 1, Name: "n", Keys: ks,
			Routes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}}})

	if got := e.routeAdvInterval(); got != 2*time.Second {
		t.Fatalf("configured interval = %v, want 2s", got)
	}
	e.SetRouteAdvInterval(0) // non-positive restores default
	if got := e.routeAdvInterval(); got != defaultRouteAdvInterval {
		t.Fatalf("default interval = %v, want %v", got, defaultRouteAdvInterval)
	}
	e.SetRouteAdvInterval(50 * time.Millisecond) // sub-second clamps to 1s
	if got := e.routeAdvInterval(); got != time.Second {
		t.Fatalf("clamped interval = %v, want 1s", got)
	}

	// shouldReadvertise must honor whatever interval it's handed, deterministically.
	ns := e.network(1)
	base := time.Now()
	if !ns.shouldReadvertise(base, 10*time.Second) {
		t.Fatal("first call should re-advertise")
	}
	if ns.shouldReadvertise(base.Add(5*time.Second), 10*time.Second) {
		t.Fatal("within interval must not re-advertise")
	}
	if !ns.shouldReadvertise(base.Add(11*time.Second), 10*time.Second) {
		t.Fatal("past interval should re-advertise")
	}
	// A shorter interval fires sooner.
	if !ns.shouldReadvertise(base.Add(13*time.Second), 1*time.Second) {
		t.Fatal("short interval should re-advertise")
	}
}
