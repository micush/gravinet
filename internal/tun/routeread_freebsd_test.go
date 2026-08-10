//go:build freebsd

package tun

import (
	"syscall"
	"testing"
)

// BSD truncates a netmask sockaddr to its significant bytes, so a /8 arrives
// carrying one byte and the rest read back as zeros. Counting leading one-bits
// has to give the right answer for both the truncated and whole forms, which
// is the part of the FreeBSD path most likely to be subtly wrong.
func TestMaskBits(t *testing.T) {
	v4 := func(a, b, c, d byte) syscall.Sockaddr {
		return &syscall.SockaddrInet4{Addr: [4]byte{a, b, c, d}}
	}
	for _, tc := range []struct {
		name string
		sa   syscall.Sockaddr
		want int
	}{
		{"/24", v4(255, 255, 255, 0), 24},
		{"/16", v4(255, 255, 0, 0), 16},
		{"/8 truncated to one byte", v4(255, 0, 0, 0), 8},
		{"/0 all zero", v4(0, 0, 0, 0), 0},
		{"/32", v4(255, 255, 255, 255), 32},
		{"/12 partial byte", v4(255, 240, 0, 0), 12},
		{"/25 partial byte", v4(255, 255, 255, 128), 25},
		{"link-layer sockaddr means host route", nil, 32},
	} {
		if got := maskBits(tc.sa, 32); got != tc.want {
			t.Errorf("%s: maskBits = %d, want %d", tc.name, got, tc.want)
		}
	}

	// v6, and the clamp against a mask longer than the family allows.
	var m6 [16]byte
	for i := 0; i < 8; i++ {
		m6[i] = 0xff
	}
	if got := maskBits(&syscall.SockaddrInet6{Addr: m6}, 128); got != 64 {
		t.Errorf("v6 /64: got %d", got)
	}
	if got := maskBits(v4(255, 255, 255, 255), 24); got != 24 {
		t.Errorf("mask longer than max should clamp, got %d", got)
	}
}
