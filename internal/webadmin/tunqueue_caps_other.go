//go:build !linux

package webadmin

// tunMultiQueueSupported — see tunqueue_caps_linux.go. Every other platform's
// internal/tun.NewMultiQueue silently opens a single queue regardless of
// what tun_queues is set to (see internal/tun/multiqueue_other.go), so the
// setting is harmless here, just inert.
const tunMultiQueueSupported = false
