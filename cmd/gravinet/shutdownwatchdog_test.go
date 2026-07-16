package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// The shutdown watchdog is what makes "restart" actually restart: because the
// systemd unit is Type=notify, `systemctl restart` waits for the old process
// to exit before starting the replacement, so a teardown step wedged on the
// kernel or a subprocess would otherwise hang the restart forever. The
// watchdog force-exits (os.Exit in production) if graceful teardown overruns.
// These tests exercise the arm/disarm race with a fake onTimeout instead of
// actually exiting.

// A clean teardown that finishes within the grace period must disarm the
// watchdog so onTimeout never runs.
func TestShutdownWatchdogDisarmedBeforeGraceDoesNotFire(t *testing.T) {
	var fired atomic.Bool
	disarm := armShutdownWatchdog(50*time.Millisecond, func() { fired.Store(true) })
	// Simulate a fast, clean teardown.
	time.Sleep(5 * time.Millisecond)
	disarm()
	// Wait well past the grace to be sure the timer branch, had it not been
	// disarmed, would have fired.
	time.Sleep(100 * time.Millisecond)
	if fired.Load() {
		t.Fatal("watchdog fired despite being disarmed before the grace elapsed — a clean shutdown must never force-exit")
	}
}

// A teardown that overruns the grace period (the wedged case) must trigger
// onTimeout — this is the guarantee that a hung shutdown still terminates the
// process so the service can restart.
func TestShutdownWatchdogFiresWhenNotDisarmed(t *testing.T) {
	fired := make(chan struct{}, 1)
	armShutdownWatchdog(20*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
		// good: the watchdog force-exited a shutdown that never completed
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire for a shutdown that overran its grace — a wedged teardown would hang the restart")
	}
}

// disarm must be idempotent and safe to call more than once (it's deferred and
// the shutdown path may be reached via multiple routes).
func TestShutdownWatchdogDisarmIdempotent(t *testing.T) {
	var fired atomic.Bool
	disarm := armShutdownWatchdog(time.Hour, func() { fired.Store(true) })
	disarm()
	disarm() // must not panic (double close) or otherwise misbehave
	disarm()
	if fired.Load() {
		t.Fatal("watchdog fired after disarm")
	}
}
