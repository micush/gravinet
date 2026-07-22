package main

import (
	"testing"
	"time"
)

// TestMergePeerCacheDropsNeverConnectedAfterGrace reproduces the actual
// problem this exists to fix: an address that was cached at some point (e.g.
// a peer briefly, or wrongly, observed at that endpoint) but has never once
// been reachable since should eventually be dropped from peer_cache, rather
// than persisting in the config forever and being retried on every restart.
func TestMergePeerCacheDropsNeverConnectedAfterGrace(t *testing.T) {
	cached := []string{"192.168.55.3:65432"}
	everConnected := map[string]bool{} // never seen live
	merged, pruned := mergePeerCache(nil, cached, everConnected, 2*time.Hour, time.Hour, peerCacheMax)
	if pruned != 1 {
		t.Fatalf("expected 1 pruned entry, got %d (merged=%v)", pruned, merged)
	}
	if len(merged) != 0 {
		t.Fatalf("stale entry should have been dropped, got merged=%v", merged)
	}
}

// TestMergePeerCacheProtectsWithinGracePeriod: a candidate that hasn't had
// staleGrace worth of uptime yet must be kept regardless of everConnected —
// it hasn't had a fair chance to prove itself either way.
func TestMergePeerCacheProtectsWithinGracePeriod(t *testing.T) {
	cached := []string{"192.168.55.3:65432"}
	everConnected := map[string]bool{}
	merged, pruned := mergePeerCache(nil, cached, everConnected, 5*time.Minute, time.Hour, peerCacheMax)
	if pruned != 0 {
		t.Fatalf("expected 0 pruned within the grace period, got %d", pruned)
	}
	if len(merged) != 1 || merged[0] != "192.168.55.3:65432" {
		t.Fatalf("candidate within grace period should be kept, got merged=%v", merged)
	}
}

// TestMergePeerCacheProtectsEverConnected: an address that was reachable at
// any point during this process's life must never be pruned for being
// currently down — that's exactly the "real peer, temporarily unreachable"
// case peer_cache exists to protect, as distinct from "never worked at all."
func TestMergePeerCacheProtectsEverConnected(t *testing.T) {
	cached := []string{"66.179.240.44:65432"}
	everConnected := map[string]bool{"66.179.240.44:65432": true}
	merged, pruned := mergePeerCache(nil, cached, everConnected, 10*time.Hour, time.Hour, peerCacheMax)
	if pruned != 0 {
		t.Fatalf("expected 0 pruned for a once-live endpoint, got %d", pruned)
	}
	if len(merged) != 1 || merged[0] != "66.179.240.44:65432" {
		t.Fatalf("once-live endpoint should be kept, got merged=%v", merged)
	}
}

// TestMergePeerCacheKeepsCurrentlyFresh: an endpoint that's part of the fresh
// (currently connected) set is always kept, regardless of everConnected —
// dedup must not require it to also appear in the historical set.
func TestMergePeerCacheKeepsCurrentlyFresh(t *testing.T) {
	fresh := []string{"10.0.0.1:65432"}
	cached := []string{"10.0.0.1:65432"} // same endpoint also happens to be cached
	merged, pruned := mergePeerCache(fresh, cached, map[string]bool{}, 10*time.Hour, time.Hour, peerCacheMax)
	if pruned != 0 {
		t.Fatalf("expected 0 pruned for a currently-fresh endpoint, got %d", pruned)
	}
	if len(merged) != 1 || merged[0] != "10.0.0.1:65432" {
		t.Fatalf("expected exactly one deduped entry, got merged=%v", merged)
	}
}

// TestMergePeerCacheFreshFirstAndCap checks the pre-existing behavior this
// refactor must not change: fresh endpoints always win a slot first, and the
// total is capped at max.
func TestMergePeerCacheFreshFirstAndCap(t *testing.T) {
	fresh := []string{"10.0.0.1:1", "10.0.0.2:1"}
	cached := []string{"10.0.0.3:1", "10.0.0.4:1", "10.0.0.5:1"}
	merged, _ := mergePeerCache(fresh, cached, map[string]bool{"10.0.0.3:1": true, "10.0.0.4:1": true, "10.0.0.5:1": true},
		10*time.Hour, time.Hour, 3)
	if len(merged) != 3 {
		t.Fatalf("expected cap of 3, got %d: %v", len(merged), merged)
	}
	if merged[0] != "10.0.0.1:1" || merged[1] != "10.0.0.2:1" {
		t.Fatalf("fresh endpoints should come first: %v", merged)
	}
}

// TestMergePeerCacheMixedPruning: a realistic mixed batch — one fresh, one
// once-live-but-currently-down, one never-live-past-grace — prunes only the
// entry that deserves it.
func TestMergePeerCacheMixedPruning(t *testing.T) {
	fresh := []string{"10.0.0.1:65432"}
	cached := []string{"10.0.0.1:65432", "10.0.0.2:65432", "192.168.55.3:65432"}
	everConnected := map[string]bool{"10.0.0.1:65432": true, "10.0.0.2:65432": true}
	merged, pruned := mergePeerCache(fresh, cached, everConnected, 10*time.Hour, time.Hour, peerCacheMax)
	if pruned != 1 {
		t.Fatalf("expected exactly 1 pruned (the never-live entry), got %d: merged=%v", pruned, merged)
	}
	want := map[string]bool{"10.0.0.1:65432": true, "10.0.0.2:65432": true}
	if len(merged) != 2 {
		t.Fatalf("expected 2 kept entries, got %v", merged)
	}
	for _, s := range merged {
		if !want[s] {
			t.Errorf("unexpected entry kept: %s", s)
		}
	}
}
