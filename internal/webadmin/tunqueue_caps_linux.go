//go:build linux

package webadmin

// tunMultiQueueSupported tells the Settings page whether tun_queues actually
// does anything on this build — see internal/tun.NewMultiQueue, Linux-only
// (IFF_MULTI_QUEUE). The config field itself is accepted on every platform
// (config.Config.TunQueues has no build tags), so a value set on a mixed-OS
// fleet doesn't error elsewhere; this only controls whether the UI presents
// the control as functional here or as a no-op.
const tunMultiQueueSupported = true
