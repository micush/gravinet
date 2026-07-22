package config

import (
	"testing"
	"time"
)

func TestKeepaliveDurationDefaultsAndFloors(t *testing.T) {
	var c Config
	if got := c.KeepaliveDuration(); got != 10*time.Second {
		t.Fatalf("unset KeepaliveInterval = %v, want 10s default", got)
	}
	c.KeepaliveInterval = 5
	if got := c.KeepaliveDuration(); got != 5*time.Second {
		t.Fatalf("KeepaliveInterval=5 -> %v, want 5s", got)
	}
	// Negative is treated the same as unset (defaults), not floored to 1s —
	// only an explicit small-but-positive value gets floored.
	c.KeepaliveInterval = -3
	if got := c.KeepaliveDuration(); got != 10*time.Second {
		t.Fatalf("negative KeepaliveInterval = %v, want 10s default", got)
	}
}

func TestPeerTimeoutDurationDefaultsAndFloors(t *testing.T) {
	var c Config
	if got := c.PeerTimeoutDuration(); got != 20*time.Second {
		t.Fatalf("unset PeerTimeout = %v, want 20s default", got)
	}
	c.PeerTimeout = 30
	if got := c.PeerTimeoutDuration(); got != 30*time.Second {
		t.Fatalf("PeerTimeout=30 -> %v, want 30s", got)
	}
}

// TestPeerTimeoutDurationClampsToKeepalive is the load-bearing behavior: an
// explicit PeerTimeout shorter than the (possibly also explicitly
// configured) KeepaliveInterval is clamped up to it, since a session that
// can time out before a single keepalive round trip completes would just
// thrash reconnecting rather than detect failure any faster.
func TestPeerTimeoutDurationClampsToKeepalive(t *testing.T) {
	c := Config{KeepaliveInterval: 15, PeerTimeout: 5}
	if got := c.PeerTimeoutDuration(); got != 15*time.Second {
		t.Fatalf("PeerTimeout=5 with KeepaliveInterval=15 -> %v, want clamped to 15s", got)
	}
	// A PeerTimeout at or above the keepalive interval is left alone.
	c2 := Config{KeepaliveInterval: 15, PeerTimeout: 45}
	if got := c2.PeerTimeoutDuration(); got != 45*time.Second {
		t.Fatalf("PeerTimeout=45 with KeepaliveInterval=15 -> %v, want 45s (no clamp needed)", got)
	}
	// Both defaulted: 20s default peer timeout already exceeds the 10s
	// default keepalive, so no clamp kicks in — this is the ordinary case.
	c3 := Config{}
	if got := c3.PeerTimeoutDuration(); got != 20*time.Second {
		t.Fatalf("both unset -> %v, want 20s (default already above default keepalive)", got)
	}
	// But raising the *keepalive* default via an explicit value past the
	// still-defaulted 20s peer timeout must still clamp the (defaulted)
	// peer timeout up — the relationship holds regardless of which side is
	// the one left at its default.
	c4 := Config{KeepaliveInterval: 25}
	if got := c4.PeerTimeoutDuration(); got != 25*time.Second {
		t.Fatalf("KeepaliveInterval=25, PeerTimeout unset -> %v, want clamped to 25s", got)
	}
}
