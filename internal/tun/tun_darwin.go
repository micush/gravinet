//go:build darwin

// macOS overlay interface via the built-in utun kernel control. The data path
// (open/read/write) is done with raw syscalls; utun frames carry a 4-byte
// address-family header that this backend adds on write and strips on read so
// the engine sees plain L3 packets. Address/MTU configuration uses ifconfig,
// which ships with macOS. This compiles for darwin but is not exercised on the
// Linux build host.
package tun

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	cAF_SYS_CONTROL   = 2
	cSYSPROTO_CONTROL = 2
	cAF_SYSTEM        = 32
	cUTUN_OPT_IFNAME  = 2
	cCTLIOCGINFO      = 0xc0644e03
	utunControlName   = "com.apple.net.utun_control"
)

type ctlInfo struct {
	ctlID   uint32
	ctlName [96]byte
}

type sockaddrCtl struct {
	scLen      uint8
	scFamily   uint8
	ssSysaddr  uint16
	scID       uint32
	scUnit     uint32
	scReserved [5]uint32
}

// Device is a macOS utun interface carrying raw L3 packets.
type Device struct {
	f    *os.File
	name string
	mtu  int
}

// New opens a utun device. If name is "utunN" that unit is requested, otherwise
// the kernel assigns one. MTU is applied and the interface is brought up.
func New(name string, mtu int) (*Device, error) {
	fd, err := syscall.Socket(cAF_SYSTEM, syscall.SOCK_DGRAM, cSYSPROTO_CONTROL)
	if err != nil {
		return nil, fmt.Errorf("utun socket: %w", err)
	}

	var info ctlInfo
	copy(info.ctlName[:], utunControlName)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), cCTLIOCGINFO, uintptr(unsafe.Pointer(&info))); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("utun CTLIOCGINFO: %v", e)
	}

	unit := 0
	if len(name) > 4 && name[:4] == "utun" {
		if n, err := strconv.Atoi(name[4:]); err == nil {
			unit = n + 1 // sc_unit is the utun number + 1
		}
	}
	sc := sockaddrCtl{
		scLen:     uint8(unsafe.Sizeof(sockaddrCtl{})),
		scFamily:  cAF_SYSTEM,
		ssSysaddr: cAF_SYS_CONTROL,
		scID:      info.ctlID,
		scUnit:    uint32(unit),
	}
	if _, _, e := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sc)), uintptr(sc.scLen)); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("utun connect: %v", e)
	}

	// Read back the assigned interface name (utunN).
	ifName := make([]byte, 32)
	ifNameLen := uintptr(len(ifName))
	if _, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd),
		cSYSPROTO_CONTROL, cUTUN_OPT_IFNAME,
		uintptr(unsafe.Pointer(&ifName[0])), uintptr(unsafe.Pointer(&ifNameLen)), 0); e != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("utun get ifname: %v", e)
	}
	assigned := string(ifName[:ifNameLen-1]) // strip trailing NUL

	_ = syscall.SetNonblock(fd, true)
	d := &Device{f: os.NewFile(uintptr(fd), assigned), name: assigned, mtu: mtu}
	if err := d.setMTU(mtu); err != nil {
		d.Close()
		return nil, err
	}
	if err := d.Up(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *Device) ifconfig(args ...string) error {
	out, err := exec.Command("ifconfig", append([]string{d.name}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s %v: %v (%s)", d.name, args, err, out)
	}
	return nil
}

func (d *Device) setMTU(mtu int) error { return d.ifconfig("mtu", strconv.Itoa(mtu)) }

func (d *Device) Up() error { return d.ifconfig("up") }

// AddIPv4 assigns a point-to-point overlay address (peer = self for /32-style
// overlay reachability), with the network's real prefix length applied as an
// explicit netmask.
//
// The netmask is not cosmetic here: on a point-to-point utun interface
// configured with identical local/dest addresses and NO netmask given, macOS
// defaults the mask to /32 (confirmed by real deployments doing this same
// local==dest setup and explicitly specifying netmask 0xffffffff for exactly
// that reason — e.g. GlobalProtect's utun). The kernel only auto-installs a
// route for whatever's covered by the interface's configured netmask, so a
// /32 here means a route to this node's own single address and nothing else —
// no route to the rest of the overlay subnet at all, meaning no way for a
// packet addressed to *any other* peer to even reach the tun device in the
// first place, let alone this node's own forwarding logic. That's the "some
// peers reachable, others not" symptom this previously produced: whichever
// peers happened to be covered by some unrelated pre-existing route worked;
// everyone else had no path to the interface at all. AddRoute (elsewhere in
// this file) only ever installs *redistributed* routes (extra subnets a peer
// advertises) — it was never a substitute for the base subnet route this
// address assignment itself needs to create.
func (d *Device) AddIPv4(addr netip.Addr, prefixLen int) error {
	return d.ifconfig(ptpIfconfigArgs(addr, prefixLen)...)
}

func (d *Device) AddIPv6(addr netip.Addr, prefixLen int) error {
	return d.ifconfig("inet6", addr.String()+"/"+strconv.Itoa(prefixLen))
}

// AddRoute installs "<prefix> -interface <name>" so the kernel hands matching
// packets to this utun. Best-effort via the route(8) command (untested here).
// macOS route(8) has no per-route metric equivalent to Linux's priority, so the
// metric is accepted for interface parity but not applied here.
func (d *Device) AddRoute(p netip.Prefix, metric int) error {
	_ = metric
	fam := "-inet"
	if p.Addr().Is6() {
		fam = "-inet6"
	}
	out, err := exec.Command("route", "-n", "add", fam, "-net", p.String(), "-interface", d.name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add %s dev %s: %v (%s)", p, d.name, err, out)
	}
	return nil
}

// DelRoute removes the route for prefix on this interface.
func (d *Device) DelRoute(p netip.Prefix, metric int) error {
	_ = metric
	fam := "-inet"
	if p.Addr().Is6() {
		fam = "-inet6"
	}
	out, err := exec.Command("route", "-n", "delete", fam, "-net", p.String(), "-interface", d.name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route delete %s dev %s: %v (%s)", p, d.name, err, out)
	}
	return nil
}

// Read returns one L3 packet, stripping the 4-byte utun address-family header.
func (d *Device) Read(p []byte) (int, error) {
	buf := make([]byte, len(p)+4)
	n, err := d.f.Read(buf)
	if err != nil {
		return 0, err
	}
	if n <= 4 {
		return 0, nil
	}
	return copy(p, buf[4:n]), nil
}

// Write prepends the 4-byte address-family header utun requires.
func (d *Device) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	af := uint32(syscall.AF_INET)
	if p[0]>>4 == 6 {
		af = uint32(syscall.AF_INET6)
	}
	buf := make([]byte, len(p)+4)
	binary.BigEndian.PutUint32(buf[:4], af)
	copy(buf[4:], p)
	n, err := d.f.Write(buf)
	if n >= 4 {
		n -= 4
	}
	return n, err
}

func (d *Device) Name() string { return d.name }

// IfIndex returns this TUN's kernel interface index — e.g. for passing as
// DefaultGateway's excludeIfIndex, so a physical-gateway lookup never
// mistakes gravinet's own tunnel-routed default for the real one. See
// gateway_darwin.go's ifIndexByName.
func (d *Device) IfIndex() (int32, error) {
	return ifIndexByName(d.name)
}
func (d *Device) MTU() int     { return d.mtu }
func (d *Device) Close() error { return d.f.Close() }
