//go:build !linux

package dhcrelay

import "fmt"

// Unreachable in practice — listenClient refuses first, and the relay does not
// start off Linux at all — but present so this file matches the interface the
// Linux build provides, the same reason listen_other.go carries a listenServer
// it will never reach.
//
// AF_PACKET is Linux's spelling. The BSDs do this through BPF and Windows not
// at all, and a relay that guessed would be guessing about where a client's
// reply goes, which is the one thing here with no safe default.
func newRawSender(_ string) (directSender, error) {
	return nil, fmt.Errorf("direct client delivery is implemented on Linux only")
}
