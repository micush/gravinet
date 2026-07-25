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
	cIFF_TUN   = 0x0001
	cIFF_NO_PI = 0x1000
	// cIFF_MULTI_QUEUE opts an interface into the kernel's multi-queue tun/tap
	// support (Linux >= 3.8): the first open with this flag creates the
	// interface as multi-queue-capable, and every subsequent open of
	// /dev/net/tun with the same name and this same flag attaches one more
	// independent queue (its own fd, its own kernel-side packet ring) to that
	// one interface, rather than failing with "device busy" the way a second
	// TUNSETIFF on a single-queue interface would. See NewMultiQueue.
	cIFF_MULTI_QUEUE = 0x0100
	cTUNSETIFF       = 0x400454ca
	cSIOCSIFMTU      = 0x8922
	cSIOCGIFFLAGS    = 0x8913
	cSIOCSIFFLAGS    = 0x8914
	cSIOCSIFADDR     = 0x8916
	cSIOCSIFNETMASK  = 0x891c
	cSIOCGIFINDEX    = 0x8933
	cSIOCSIFTXQLEN   = 0x8943
	cIFF_UP          = 0x1
	cIFF_RUNNING     = 0x40

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
// SOCK_CLOEXEC even though every caller here closes it well before returning
// (defer syscall.Close right after) — a concurrent exec.Command on another
// goroutine could still fork+exec in the brief window this fd is open and
// inherit it otherwise; atomic-at-open avoids that race entirely rather than
// relying on the close always winning it.
func ctlSocket(family int) (int, error) {
	return syscall.Socket(family, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
}

// openTunQueue opens one /dev/net/tun fd and binds it to name via TUNSETIFF,
// optionally as a multi-queue queue (see cIFF_MULTI_QUEUE). If name is empty
// the kernel assigns one (tunN) and assigned reports what it picked — every
// later queue on the same interface must pass that name back in, not "".
// Shared by New (single queue) and NewMultiQueue (queue 0 plus n-1 more).
func openTunQueue(name string, multiQueue bool) (f *os.File, assigned string, err error) {
	// O_CLOEXEC matters here specifically because this fd lives for the whole
	// daemon lifetime: every exec.Command gravinet ever runs (useradd,
	// chpasswd, hostnamectl, sysrc, ...) forks from this process, and without
	// it every one of those children would inherit an open handle to
	// /dev/net/tun. On an SELinux-enforcing host that's exactly what makes
	// chpasswd trip an AVC denial for a chr_file access it never actually
	// asked for — it just inherited gravinet's own fd. Passed atomically at
	// open time (not a separate fcntl afterward) so there's no window where a
	// concurrent exec.Command on another goroutine could still race in and
	// inherit it.
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w (need CAP_NET_ADMIN)", err)
	}
	var req [ifreqSize]byte
	copy(req[:ifnameSize], name)
	flags := uint16(cIFF_TUN | cIFF_NO_PI)
	if multiQueue {
		flags |= cIFF_MULTI_QUEUE
	}
	binary.NativeEndian.PutUint16(req[ifnameSize:], flags)
	if err := ioctl(uintptr(fd), cTUNSETIFF, unsafe.Pointer(&req[0])); err != nil {
		syscall.Close(fd)
		return nil, "", fmt.Errorf("TUNSETIFF: %w", err)
	}
	// Non-blocking + os.NewFile registers the fd with Go's network poller, so a
	// blocked Read is interruptible by Close (clean shutdown).
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, "", fmt.Errorf("set nonblock: %w", err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), string(trimZero(req[:ifnameSize])), nil
}

// New creates a TUN device. If name is empty the kernel assigns one (tunN).
// It sets the MTU and brings the interface up. Addresses are assigned
// separately via AddIPv4/AddIPv6 once the overlay address is chosen.
func New(name string, mtu int) (*Device, error) {
	f, assigned, err := openTunQueue(name, false)
	if err != nil {
		return nil, err
	}
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

// Queue is one additional multi-queue TUN queue beyond a Device's own
// Read/Write (queue 0) — see NewMultiQueue. Read-only from the mesh engine's
// point of view: every inbound (decrypted) packet is written back through
// the Device itself, not through a Queue, so Queue only ever needs Read.
type Queue struct{ f *os.File }

// Read returns one IP packet from this queue.
func (q *Queue) Read(p []byte) (int, error) { return q.f.Read(p) }

// Close releases this queue's fd. The interface itself (and its other
// queues) survive; a multi-queue tun interface is only actually torn down
// when its last open fd closes.
func (q *Queue) Close() error { return q.f.Close() }

// NewMultiQueue creates a TUN device with n independent queues sharing one
// interface — n-1 of them returned separately as *Queue, the first as the
// usual *Device (queue 0, exactly what New returns). Each queue is its own
// fd and kernel-side packet ring; the kernel spreads packets across them by
// flow hash, the same way it spreads a REUSEPORT socket set's datagrams
// across sockets (see transport.startWorkers) or a multi-queue NIC's
// traffic across RX/TX rings. That's what makes this useful: gravinet's
// outbound path is otherwise exactly one goroutine doing one read() syscall
// per packet for an entire network's originated traffic, no matter how many
// cores are free — see tunLoop's doc comment in internal/mesh/engine.go.
// Multiple queues turn that into n independent goroutines, each with its own
// fd, actually able to run in parallel.
//
// n<=1 behaves identically to New: single fd, no IFF_MULTI_QUEUE, nil Queue
// slice. This is deliberately opt-in (config's tun_queues, 0/1 = off) rather
// than defaulted on — unlike the outbound worker pool or UDP batching, which
// only add concurrency *after* a single read, this changes how the read
// itself is done, and gravinet's history with data-plane changes in that
// category (see docs/changelog.md's Phase B/C entries) is why that
// distinction matters enough to default conservatively here too.
//
// A single flow (one TCP connection, most kernels' flow-hash queue
// selection) will still land on one queue — this helps aggregate throughput
// across many concurrent flows, not a single stream's ceiling.
func NewMultiQueue(name string, mtu, n int) (*Device, []*Queue, error) {
	if n <= 1 {
		d, err := New(name, mtu)
		return d, nil, err
	}
	f0, assigned, err := openTunQueue(name, true)
	if err != nil {
		return nil, nil, err
	}
	d := &Device{f: f0, name: assigned, mtu: mtu}
	if err := d.setMTU(mtu); err != nil {
		f0.Close()
		return nil, nil, err
	}
	_ = d.setTxQueueLen(defaultTxQueueLen)
	if err := d.Up(); err != nil {
		f0.Close()
		return nil, nil, err
	}

	queues := make([]*Queue, 0, n-1)
	for i := 1; i < n; i++ {
		fq, _, err := openTunQueue(assigned, true)
		if err != nil {
			// All-or-nothing: an interface with fewer queues than the caller
			// asked for and expects to spawn readers against is worse than
			// no interface at all — close everything opened so far, including
			// queue 0, rather than hand back a partially multi-queue Device.
			for _, q := range queues {
				q.Close()
			}
			d.Close()
			return nil, nil, fmt.Errorf("open queue %d/%d on %s: %w", i+1, n, assigned, err)
		}
		queues = append(queues, &Queue{f: fq})
	}
	return d, queues, nil
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
