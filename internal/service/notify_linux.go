//go:build linux

package service

import (
	"net"
	"os"
)

// NotifyReady sends READY=1 to systemd via the sd_notify protocol when the
// daemon is launched under a Type=notify unit (NOTIFY_SOCKET is set). It is a
// no-op otherwise, and needs no libsystemd — just a datagram to the socket.
func NotifyReady() error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// A leading '@' denotes an abstract socket (replace with NUL).
	name := addr
	if name[0] == '@' {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1"))
	return err
}
