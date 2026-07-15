//go:build linux

package transport

import (
	"net"
	"syscall"

	"gravinet/internal/logx"
)

// setSocketBuffers enlarges the receive and send buffers on a bound UDP socket.
// It first tries SO_RCVBUFFORCE/SO_SNDBUFFORCE, which (with CAP_NET_ADMIN — the
// daemon runs as root) bypass the net.core.{r,w}mem_max clamp, so the operator
// does not have to raise sysctls. If the force variant is not permitted it falls
// back to the ordinary option, which the kernel clamps to the configured max.
// Buffer sizing is an optimization, so failures are logged and never fatal.
func setSocketBuffers(c *net.UDPConn, size int, log *logx.Logger) {
	if size <= 0 {
		return
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		setOne(int(fd), syscall.SO_RCVBUFFORCE, syscall.SO_RCVBUF, size, "rcvbuf", log)
		setOne(int(fd), syscall.SO_SNDBUFFORCE, syscall.SO_SNDBUF, size, "sndbuf", log)
	})
}

func setOne(fd, forceOpt, plainOpt, size int, name string, log *logx.Logger) {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, forceOpt, size); err == nil {
		return
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, plainOpt, size); err != nil && log != nil {
		log.Debugf("transport: set %s to %d failed: %v", name, size, err)
	}
}
