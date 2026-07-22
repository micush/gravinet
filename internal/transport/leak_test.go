package transport

import (
	"net/netip"
	"runtime"
	"testing"
	"time"
)

// TestNoGoroutineLeak opens and closes many transports in a row — with
// batching forced on, so both the read workers and the per-socket flusher
// goroutines this session added are exercised — and checks the goroutine
// count returns to baseline. Close() is supposed to be the one place that
// unwinds all of it (readLoopBatched via socket closure unblocking
// SyscallConn.Read, flushers via stopBatch's close(stopFlush) + flushWG.Wait);
// this is the test that would catch a flusher blocked forever on an unsent
// wakeup, or a read worker not unblocking when its socket closes.
func TestNoGoroutineLeak(t *testing.T) {
	forceBatchable(t)
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 30; i++ {
		tr, err := Open(Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 2,
			Handler: func([]byte, netip.AddrPort, Family) {},
		})
		if err != nil {
			t.Fatalf("iter %d: open: %v", i, err)
		}
		// Queue traffic on every socket so every flusher actually has
		// something to drain before Close, rather than sitting idle.
		dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(tr.Port()))
		for j := 0; j < 20; j++ {
			_ = tr.Send(dst, []byte("leak-check"))
		}
		if err := tr.Close(); err != nil {
			t.Fatalf("iter %d: close: %v", i, err)
		}
	}

	// Goroutines wind down asynchronously (the runtime scheduler, GC,
	// netpoller); poll briefly rather than assert instantly.
	var after int
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= base+2 || time.Now().After(deadline) { // small slack for background runtime goroutines
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > base+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: started at %d, ended at %d after 30 open/close cycles\n%s", base, after, buf[:n])
	}
}
