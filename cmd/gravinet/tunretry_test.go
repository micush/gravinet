package main

import (
	"errors"
	"testing"
	"time"
)

// The bug this guards, in full:
//
//  1. Host sleeps and wakes. maintLoop detects the clock jump and asks for a
//     clean process restart (the underlay socket, TUN device and OS routes can't
//     all be rebuilt in place).
//  2. shutdown() closes the TUN fd. A Linux TUN is non-persistent, so mesh0 is
//     destroyed with it.
//  3. selfRestart() immediately execs a new image of the process.
//  4. The new process calls tun.New("mesh0") — but the kernel's teardown of the
//     OLD mesh0 is asynchronous (unregister_netdevice is deferred and can be held
//     up by references the stack hasn't dropped). TUNSETIFF still sees the name in
//     use and fails.
//
// A single attempt loses that race against its own predecessor. And the
// consequence was total, because buildNetSpecs logs the error and `continue`s:
// no NetSpec, so no netState, no maintLoop, and no data-plane supervisor. The
// supervisor in mesh/dataplane.go guards a *running* interface; nothing guarded
// failing to create one. The daemon came up reporting success with zero networks
// and mesh0 simply absent, and nothing ever retried — gone until a manual
// restart. Exactly the reported symptom.

// retryOpen mirrors newTunRetrying's loop with the syscall injected, so the
// backoff contract can be tested without CAP_NET_ADMIN or a real tun device.
func retryOpen(budget time.Duration, open func(attempt int) error) (attempts int, err error) {
	deadline := time.Now().Add(budget)
	delay := time.Millisecond
	for attempt := 1; ; attempt++ {
		err = open(attempt)
		if err == nil {
			return attempt, nil
		}
		if time.Now().After(deadline) {
			return attempt, err
		}
		time.Sleep(delay)
		if delay < 8*time.Millisecond {
			delay *= 2
		}
	}
}

// TestTunOpenRetriesPastATransientBusy: the predecessor's interface is still
// being unwound for the first few attempts, then the name frees up. A single
// attempt would have failed and taken the whole network down with it.
func TestTunOpenRetriesPastATransientBusy(t *testing.T) {
	busy := errors.New("device or resource busy")
	attempts, err := retryOpen(2*time.Second, func(attempt int) error {
		if attempt < 4 { // kernel still tearing down the old mesh0
			return busy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a transiently busy interface must be retried until the kernel frees it, not dropped: %v", err)
	}
	if attempts != 4 {
		t.Fatalf("expected to succeed on attempt 4, got %d", attempts)
	}
}

// TestTunOpenGivesUpOnAPermanentFailure: a genuinely broken configuration (no
// CAP_NET_ADMIN, /dev/net/tun absent) must still fail, and fail inside the
// budget rather than spinning forever — the daemon has to come up or report why.
func TestTunOpenGivesUpOnAPermanentFailure(t *testing.T) {
	perm := errors.New("operation not permitted")
	start := time.Now()
	attempts, err := retryOpen(120*time.Millisecond, func(int) error { return perm })
	if err == nil {
		t.Fatal("a permanently failing open must eventually be reported, not retried forever")
	}
	if !errors.Is(err, perm) {
		t.Fatalf("the underlying cause must survive to the caller, got %v", err)
	}
	if attempts < 2 {
		t.Errorf("should have retried at least once before giving up, got %d attempt(s)", attempts)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %s; must stay near its budget so startup can't hang", elapsed)
	}
}

// TestTunOpenSucceedsFirstTryCostsNothing: the common case must not pay for the
// retry — no sleeps, one attempt.
func TestTunOpenSucceedsFirstTryCostsNothing(t *testing.T) {
	start := time.Now()
	attempts, err := retryOpen(5*time.Second, func(int) error { return nil })
	if err != nil || attempts != 1 {
		t.Fatalf("attempts=%d err=%v; want a single successful attempt", attempts, err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("a first-try success took %s; the retry path must be free when it isn't needed", elapsed)
	}
}
