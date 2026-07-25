//go:build linux && (amd64 || arm64)

package webadmin

// udpGSOSupported tells the Settings page whether udp_gso actually does
// anything on this build — see internal/transport/gso_linux.go, gated to
// linux/amd64 and linux/arm64 (the only architectures its hand-built cmsg
// layout has been pinned for; see TestMmsghdrLayout in that package). Also
// requires Phase A batching to be active (GOMAXPROCS>=2) at runtime, which
// this build-time flag can't see — the Settings row's description covers
// that caveat in words, since there's no clean way to surface a runtime
// condition in a capability flag computed at compile time.
const udpGSOSupported = true
