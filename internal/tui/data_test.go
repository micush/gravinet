package tui

// A regression test for a real race the type checker and the normal test
// suite both missed: app.go's periodic tick used to call snap.loadLive()
// directly on the model's live snapshot, mutating its fields in place while
// a lazy fetch (lazy.go) started from a page build could be reading that
// same snapshot from a goroutine of its own. -race did not catch it in the
// ordinary test run because nothing in app_test.go drives a real concurrent
// fetch against a real refresh with timing that lines up — which is exactly
// the situation a data race waits for the worst possible moment to occur in
// production instead. This test manufactures that timing on purpose.

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

func TestRefreshedLiveDoesNotRaceAConcurrentLazyFetch(t *testing.T) {
	snap := &snapshot{
		sockPath: "/nonexistent-for-this-test",
		ifaces:   []mesh.IfaceInfo{{Name: "corp", Iface: "mesh0"}},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// One goroutine standing in for a lazy fetch that reads the snapshot it
	// was handed at page-build time — pageMetrics does exactly this with
	// s.ifaces, held across the roughly one-second TakeHostSnapshot call.
	// daemonErr is read alongside it because it is the one field loadLive
	// always writes, even when the dial fails immediately (as it does here,
	// against a socket that does not exist) — ifaces is only reached after a
	// successful "peers" round trip, and a test against a fake socket would
	// never touch it, silently testing nothing.
	held := snap
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = len(held.ifaces)  // the read that used to race
			_ = held.daemonErr    // written on every call, success or failure
			time.Sleep(time.Microsecond)
		}
	}()

	// The other goroutine standing in for app.run's ticker, replacing the
	// model's snapshot the way the fix does: by producing a new value rather
	// than mutating the one the fetch goroutine above is holding.
	//
	// A dial to a socket that does not exist fails almost instantly, so
	// without runtime.Gosched a tight loop here can finish all 200 iterations
	// before the reader goroutine above is even scheduled for the first
	// time — which would mean the two never actually overlap on the same
	// memory, and the race detector (which tracks a happens-before relation
	// over the execution that actually occurred, not over what could occur)
	// would have nothing to report. Yielding after each call is what makes
	// the interleaving this test exists to catch actually happen.
	cur := snap
	for i := 0; i < 2000; i++ {
		cur = cur.refreshedLive()
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()

	// The original object the fetch goroutine held must be exactly as
	// constructed — refreshedLive must not have touched it.
	if len(snap.ifaces) != 1 || snap.ifaces[0].Iface != "mesh0" {
		t.Errorf("refreshedLive mutated the snapshot it was called on: %+v", snap.ifaces)
	}
}
