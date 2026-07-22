//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package webadmin

import (
	"fmt"
	"time"
)

const captureSupported = false

// startCapture has no backend on this platform (only Linux/AF_PACKET, macOS,
// FreeBSD, and OpenBSD's BPF, and Windows/Npcap are implemented); the UI
// shows an explanatory message.
func startCapture(ifaceName string, snaplen int, onPacket func(time.Time, []byte)) (capHandle, int, error) {
	return nil, -1, fmt.Errorf("packet capture is not supported on this platform")
}
