package config

import (
	"sync"
	"testing"
	"time"
)

// TestWithLockAndLockSerializeAgainstEachOther is the regression test for the
// actual bug this pair of functions fixes: two independent writers of the
// same config file — modeled here on the web admin's mutateConfig (via
// WithLock) and the engine's persist hook (via the raw Lock, since that
// caller's existing early-return control flow doesn't fit a func() error
// shape) — used to have no shared lock at all. Each serialized only against
// itself, so one could load its own copy, the other could save a change in
// between, and the first would then save its (now stale) copy over it,
// silently reverting the other's change with no error anywhere.
//
// This starts both writers at (as close to) the same instant as possible,
// each touching a *different* field, with a deliberate delay between each
// one's load and save so a real race has time to manifest if the lock isn't
// actually shared. If WithLock and Lock are the same underlying per-path
// lock, one writer's whole load-mutate-save cycle completes before the other
// even loads, so both fields survive; if they aren't (the pre-fix bug),
// whichever saves second wins and the first writer's field is silently lost.
func TestWithLockAndLockSerializeAgainstEachOther(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfg := Default()
	cfg.PrimaryPort = 51820
	cfg.EnableIPv4 = true
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer A (WithLock, mirroring mutateConfig): sets PrimaryPort.
	go func() {
		defer wg.Done()
		if err := WithLock(path, func() error {
			c, err := Load(path)
			if err != nil {
				return err
			}
			time.Sleep(50 * time.Millisecond) // give writer B a chance to race
			c.PrimaryPort = 12345
			return c.SaveTo(path)
		}); err != nil {
			t.Errorf("writer A: %v", err)
		}
	}()

	// Writer B (raw Lock, mirroring the persist hook): sets TCPFallbackPort.
	go func() {
		defer wg.Done()
		l := Lock(path)
		l.Lock()
		defer l.Unlock()
		c, err := Load(path)
		if err != nil {
			t.Errorf("writer B load: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond) // give writer A a chance to race
		c.TCPFallbackPort = 8443
		if err := c.SaveTo(path); err != nil {
			t.Errorf("writer B save: %v", err)
		}
	}()

	wg.Wait()

	final, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.PrimaryPort != 12345 {
		t.Errorf("PrimaryPort = %d, want 12345 (writer A's change was lost)", final.PrimaryPort)
	}
	if final.TCPFallbackPort != 8443 {
		t.Errorf("TCPFallbackPort = %d, want 8443 (writer B's change was lost)", final.TCPFallbackPort)
	}
}

// TestLockReturnsSameMutexForSamePath proves Lock and WithLock actually share
// state (the whole fix depends on this): the same path must always yield the
// identical *sync.Mutex instance, and a different path must yield a different
// one (otherwise every config file in the process would serialize against
// every other, unnecessarily).
func TestLockReturnsSameMutexForSamePath(t *testing.T) {
	a1 := Lock("/tmp/one.json")
	a2 := Lock("/tmp/one.json")
	if a1 != a2 {
		t.Fatal("Lock returned different mutexes for the same path")
	}
	b := Lock("/tmp/two.json")
	if a1 == b {
		t.Fatal("Lock returned the same mutex for two different paths")
	}
}
