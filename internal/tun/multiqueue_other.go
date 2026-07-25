//go:build !linux

package tun

// Queue exists on every platform so cmd/gravinet's device-construction code
// doesn't need per-OS build tags of its own — but only Linux's tun_linux.go
// ever actually populates one. See NewMultiQueue below.
type Queue struct{}

// Read is never called: NewMultiQueue on this platform always returns a nil
// Queue slice, so nothing ever holds a non-nil *Queue to call it on.
func (*Queue) Read(p []byte) (int, error) { return 0, nil }

// Close is likewise never reached; present only so *Queue's shape matches
// the Linux one closely enough that callers don't need a build-tagged type
// switch.
func (*Queue) Close() error { return nil }

// NewMultiQueue on this platform only ever opens a single queue — there is
// no IFF_MULTI_QUEUE equivalent wired here yet. queues>1 is accepted rather
// than rejected: an operator running tun_queues on a mixed-OS fleet gets one
// queue on this host (identical to tun_queues unset) instead of a config
// error that would only ever fire on platforms that can't take advantage of
// the setting anyway.
func NewMultiQueue(name string, mtu, queues int) (*Device, []*Queue, error) {
	d, err := New(name, mtu)
	return d, nil, err
}
