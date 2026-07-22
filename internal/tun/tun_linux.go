//go:build linux

// Package tun provides the platform overlay interface. The Linux backend talks
// to /dev/net/tun and configures MTU, flags, and addresses through raw ioctls,
// so it needs no external command (`ip`/`ifconfig`) and no cgo.
package tun

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"syscall"
	"unsafe"
)

// ioctl request numbers and flag bits (Linux, asm-generic).
const (
	cIFF_TUN        = 0x0001
	cIFF_NO_PI      = 0x1000
	cTUNSETIFF      = 0x400454ca
	cSIOCSIFMTU     = 0x8922
	cSIOCGIFFLAGS   = 0x8913
	cSIOCSIFFLAGS   = 0x8914
	cSIOCSIFADDR    = 0x8916
	cSIOCSIFNETMASK = 0x891c
	cSIOCGIFINDEX   = 0x8933
	cSIOCSIFTXQLEN  = 0x8943
	cIFF_UP         = 0x1
	cIFF_RUNNING    = 0x40

	ifnameSize = 16
	ifreqSize  = 40 // sizeof(struct ifreq) on 64-bit

	// defaultTxQueueLen deepens the interface queue past the 500-packet default so
	// brief stalls in the single overlay reader don't drop outbound packets.
	defaultTxQueueLen = 1000
)

// Device is a Linux TUN interface carrying raw L3 packets (IFF_NO_PI).
type Device struct {
	f    *os.File
	name string
	mtu  int
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// ctlSocket opens a throwaway datagram socket for SIOC* interface ioctls.
func ctlSocket(family int) (int, error) {
	return syscall.Socket(family, syscall.SOCK_DGRAM, 0)
}

// New creates a TUN device. If name is empty the kernel assigns one (tunN).
// It sets the MTU and brings the interface up. Addresses are assigned
// separately via AddIPv4/AddIPv6 once the overlay address is chosen.
func New(name string, mtu int) (*Device, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w (need CAP_NET_ADMIN)", err)
	}
	var req [ifreqSize]byte
	copy(req[:ifnameSize], name)
	binary.NativeEndian.PutUint16(req[ifnameSize:], cIFF_TUN|cIFF_NO_PI)
	if err := ioctl(uintptr(fd), cTUNSETIFF, unsafe.Pointer(&req[0])); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF: %w", err)
	}
	// Non-blocking + os.NewFile registers the fd with Go's network poller, so a
	// blocked Read is interruptible by Close (clean shutdown).
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set nonblock: %w", err)
	}
	f := os.NewFile(uintptr(fd), "/dev/net/tun")

	assigned := string(trimZero(req[:ifnameSize]))
	d := &Device{f: f, name: assigned, mtu: mtu}
	if err := d.setMTU(mtu); err != nil {
		f.Close()
		return nil, err
	}
	// Deepen the interface tx queue so brief stalls in the single overlay reader
	// don't overflow the default 500-packet qdisc and drop outbound packets.
	// Best-effort: a failure here only forgoes the optimization.
	_ = d.setTxQueueLen(defaultTxQueueLen)
	if err := d.Up(); err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

// setTxQueueLen sets the interface transmit queue length (in packets).
func (d *Device) setTxQueueLen(n int) error {
	s, err := ctlSocket(syscall.AF_INET)
	if err != nil {
		return err
	}
	defer syscall.Close(s)
	req := d.ifreqWithName()
	binary.NativeEndian.PutUint32(req[ifnameSize:], uint32(n))
	if err := ioctl(uintptr(s), cSIOCSIFTXQLEN, unsafe.Pointer(&req[0])); err != nil {
		return fmt.Errorf("set txqueuelen: %w", err)
	}
	return nil
}

func trimZero(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// ifreqWithName returns a zeroed ifreq buffer with the interface name set.
func (d *Device) ifreqWithName() [ifreqSize]byte {
	var req [ifreqSize]byte
	copy(req[:ifnameSize], d.name)
	return req
}

func (d *Device) setMTU(mtu int) error {
	s, err := ctlSocket(syscall.AF_INET)
	if err != nil {
		return err
	}
	defer syscall.Close(s)
	req := d.ifreqWithName()
	binary.NativeEndian.PutUint32(req[ifnameSize:], uint32(mtu))
	if err := ioctl(uintptr(s), cSIOCSIFMTU, unsafe.Pointer(&req[0])); err != nil {
		return fmt.Errorf("set mtu: %w", err)
	}
	d.mtu = mtu
	return nil
}

// Up brings the interface administratively up and running.
func (d *Device) Up() error {
	s, err := ctlSocket(syscall.AF_INET)
	if err != nil {
		return err
	}
	defer syscall.Close(s)
	req := d.ifreqWithName()
	if err := ioctl(uintptr(s), cSIOCGIFFLAGS, unsafe.Pointer(&req[0])); err != nil {
		return fmt.Errorf("get flags: %w", err)
	}
	flags := binary.NativeEndian.Uint16(req[ifnameSize:])
	flags |= cIFF_UP | cIFF_RUNNING
	binary.NativeEndian.PutUint16(req[ifnameSize:], flags)
	if err := ioctl(uintptr(s), cSIOCSIFFLAGS, unsafe.Pointer(&req[0])); err != nil {
		return fmt.Errorf("set flags up: %w", err)
	}
	return nil
}

// AddIPv4 assigns an IPv4 address and prefix to the interface.
func (d *Device) AddIPv4(addr netip.Addr, prefixLen int) error {
	if !addr.Is4() {
		return fmt.Errorf("AddIPv4: %s is not IPv4", addr)
	}
	s, err := ctlSocket(syscall.AF_INET)
	if err != nil {
		return err
	}
	defer syscall.Close(s)

	// SIOCSIFADDR with sockaddr_in at offset 16.
	req := d.ifreqWithName()
	a4 := addr.As4()
	binary.NativeEndian.PutUint16(req[ifnameSize:], syscall.AF_INET) // sin_family
	copy(req[ifnameSize+4:ifnameSize+8], a4[:])                      // sin_addr at offset +4
	if err := ioctl(uintptr(s), cSIOCSIFADDR, unsafe.Pointer(&req[0])); err != nil {
		return fmt.Errorf("set v4 addr: %w", err)
	}

	// Netmask.
	mask := prefixToMask4(prefixLen)
	reqm := d.ifreqWithName()
	binary.NativeEndian.PutUint16(reqm[ifnameSize:], syscall.AF_INET)
	copy(reqm[ifnameSize+4:ifnameSize+8], mask[:])
	if err := ioctl(uintptr(s), cSIOCSIFNETMASK, unsafe.Pointer(&reqm[0])); err != nil {
		return fmt.Errorf("set v4 netmask: %w", err)
	}
	return nil
}

func prefixToMask4(prefix int) [4]byte {
	var m uint32
	if prefix > 0 {
		m = ^uint32(0) << (32 - prefix)
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], m)
	return b
}

// AddIPv6 assigns an IPv6 address and prefix to the interface using the
// in6_ifreq structure on an AF_INET6 control socket.
func (d *Device) AddIPv6(addr netip.Addr, prefixLen int) error {
	if !addr.Is6() || addr.Is4In6() {
		return fmt.Errorf("AddIPv6: %s is not IPv6", addr)
	}
	s, err := ctlSocket(syscall.AF_INET6)
	if err != nil {
		return err
	}
	defer syscall.Close(s)

	// First resolve the interface index.
	reqIdx := d.ifreqWithName()
	if err := ioctl(uintptr(s), cSIOCGIFINDEX, unsafe.Pointer(&reqIdx[0])); err != nil {
		return fmt.Errorf("get ifindex: %w", err)
	}
	ifindex := int32(binary.NativeEndian.Uint32(reqIdx[ifnameSize:]))

	// struct in6_ifreq { in6_addr(16); u32 prefixlen; int ifindex; }
	var in6 [24]byte
	a16 := addr.As16()
	copy(in6[0:16], a16[:])
	binary.NativeEndian.PutUint32(in6[16:20], uint32(prefixLen))
	binary.NativeEndian.PutUint32(in6[20:24], uint32(ifindex))
	if err := ioctl(uintptr(s), cSIOCSIFADDR, unsafe.Pointer(&in6[0])); err != nil {
		return fmt.Errorf("set v6 addr: %w", err)
	}
	return nil
}

// Read returns one IP packet from the interface.
func (d *Device) Read(p []byte) (int, error) { return d.f.Read(p) }

// Write injects one IP packet into the interface.
func (d *Device) Write(p []byte) (int, error) { return d.f.Write(p) }

// Name reports the interface name.
func (d *Device) Name() string { return d.name }

// MTU reports the configured MTU.
func (d *Device) MTU() int { return d.mtu }

// Close tears down the interface.
func (d *Device) Close() error { return d.f.Close() }
