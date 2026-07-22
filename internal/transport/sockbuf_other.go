//go:build !linux

package transport

import (
	"net"

	"gravinet/internal/logx"
)

// setSocketBuffers enlarges the receive and send buffers on a bound UDP socket
// using the portable net.UDPConn methods. On these platforms the kernel may clamp
// the value to a system maximum; buffer sizing is best-effort and never fatal.
func setSocketBuffers(c *net.UDPConn, size int, log *logx.Logger) {
	if size <= 0 {
		return
	}
	if err := c.SetReadBuffer(size); err != nil && log != nil {
		log.Debugf("transport: set rcvbuf to %d failed: %v", size, err)
	}
	if err := c.SetWriteBuffer(size); err != nil && log != nil {
		log.Debugf("transport: set sndbuf to %d failed: %v", size, err)
	}
}
