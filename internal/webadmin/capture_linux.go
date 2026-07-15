//go:build linux

package webadmin

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

const captureSupported = true

func htons(v uint16) uint16 { return (v << 8) | (v >> 8) }

type linuxCapture struct {
	f       *os.File
	stopped atomic.Bool
}

// startCapture opens an AF_PACKET raw socket bound to ifaceName and streams each
// received frame to onPacket until stop() is called. Requires CAP_NET_RAW.
// The returned linktype is always -1 (no override): the caller already picked
// one via linktypeForIface before calling startCapture, which matches what
// AF_PACKET actually delivers (Ethernet framing, or bare IP for TUN-style
// point-to-point interfaces).
func startCapture(ifaceName string, snaplen int, onPacket func(time.Time, []byte)) (capHandle, int, error) {
	ifi, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, -1, fmt.Errorf("interface %q: %w", ifaceName, err)
	}
	proto := htons(uint16(syscall.ETH_P_ALL))
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(proto))
	if err != nil {
		return nil, -1, fmt.Errorf("open raw socket (need root/CAP_NET_RAW?): %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		syscall.Close(fd)
		return nil, -1, fmt.Errorf("bind %q: %w", ifaceName, err)
	}
	_ = syscall.SetNonblock(fd, true)
	h := &linuxCapture{f: os.NewFile(uintptr(fd), "gravinet-capture")}
	go h.loop(snaplen, onPacket)
	return h, -1, nil
}

func (h *linuxCapture) stop() {
	h.stopped.Store(true)
	if h.f != nil {
		h.f.Close() // unblocks a pending Read
	}
}

func (h *linuxCapture) loop(snaplen int, onPacket func(time.Time, []byte)) {
	buf := make([]byte, snaplen)
	for {
		if h.stopped.Load() {
			return
		}
		// A short deadline lets the loop notice stop() even on an idle interface.
		_ = h.f.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := h.f.Read(buf)
		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			return // closed or fatal
		}
		if n <= 0 || h.stopped.Load() {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		onPacket(time.Now(), pkt)
	}
}
