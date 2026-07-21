//go:build freebsd

// FreeBSD overlay interface via the built-in tun(4) kernel driver, opened in
// "multi-af" mode (TUNSIFHEAD) so IPv4 and IPv6 share one device the same way
// Windows' Wintun and macOS's utun already do here: each packet is prefixed
// with a 4-byte address family in network byte order, which this backend
// adds on write and strips on read (see tun(4) and tun_darwin.go, whose
// utun framing is the same shape). Address/MTU/route configuration uses
// ifconfig/route(8), which ship with the base system, matching the style of
// the macOS backend rather than reaching for raw SIOCSIFADDR-family ioctls.
//
// The ioctl numbers below aren't in Go's standard syscall package for this
// GOOS, so they're computed from FreeBSD's own <net/if_tun.h> and
// <sys/sockio.h> _IOW/_IOWR macros (the same BSD ioctl encoding used by
// tun_darwin.go's CTLIOCGINFO): direction|((len&0x1fff)<<16)|(group<<8)|num.
// Cross-checked against wireguard-go's tun_freebsd.go, which independently
// arrives at the same values for the tun(4)-specific ones.
//
// This compiles for freebsd/amd64 but is not exercised on the Linux build
// host, same caveat as the Windows and macOS backends.
package tun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

const (
	ifNameSize = 16 // IFNAMSIZ

	tunGIFName    = 0x4020745d // TUNGIFNAME: get the kernel-assigned tunN name (FreeBSD 12.1+)
	tunSIFHead    = 0x80047460 // TUNSIFHEAD: enable 4-byte-address-family framing
	tunSIFMode    = 0x8004745e // TUNSIFMODE: set IFF_BROADCAST|IFF_MULTICAST (out of point-to-point)
	tunSIFPid     = 0x2000745f // TUNSIFPID: become the controlling process for this tun
	siocSIFName   = 0x80206928 // SIOCSIFNAME: rename an interface — _IOW('i',40,struct ifreq)
	siocIFDestroy = 0x80206979 // SIOCIFDESTROY: destroy a cloned interface — _IOW('i',121,struct ifreq)
)

// Device is a FreeBSD tun(4) interface carrying raw L3 packets.
type Device struct {
	f    *os.File
	name string
	mtu  int
	// rbuf backs Read, reused across calls rather than allocated fresh each
	// time — see Read's comment for why this matters. Safe without a lock:
	// Read is only ever called from the single dedicated goroutine
	// mesh.Engine runs per network.
	rbuf []byte
}

// ifreqName is struct ifreq truncated to just the name (32 bytes total,
// matching FreeBSD's ABI): used both to read back TUNGIFNAME's result and to
// address an interface by name for SIOCIFDESTROY.
type ifreqName struct {
	Name [ifNameSize]byte
	_    [16]byte
}

// ifreqData is struct ifreq's name-plus-pointer shape, used for SIOCSIFNAME
// (whose payload is a pointer to the new name, not the name inline).
type ifreqData struct {
	Name [ifNameSize]byte
	Data uintptr
	_    [16 - 8]byte // pad to 32 bytes; 8 == sizeof(uintptr) on amd64/arm64
}

// New opens a fresh tun(4) device (the kernel clones the next free tunN),
// puts it into multi-af mode, and — if name is non-empty — renames it to the
// requested name, so gravinet's networks get the same predictable interface
// names here (e.g. "mesh0") as they do on every other platform.
//
// Deliberately does not pre-check whether name is already in use (an earlier
// version did, via net.InterfaceByName) — that check treated a live re-enable
// differently from a fresh boot for no good reason, since SIOCSIFNAME below
// already fails naturally (EEXIST-equivalent) if the name really is taken,
// giving the same protection without a separate check that could misfire on
// its own. Neither the Darwin nor Windows TUN backends have an equivalent
// pre-check either.
func New(name string, mtu int) (*Device, error) {
	f, err := os.OpenFile("/dev/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/tun: %w", err)
	}
	fd := f.Fd()

	// Non-blocking is what makes a concurrent Close() actually interrupt a
	// Read() in progress — see the identical comment and call in
	// tun_linux.go's New(). Without it, tunLoop's dev.Read() (internal/mesh's
	// engine.go) is a genuine blocking syscall: engine.Stop() closes the
	// device to unblock that Read so it can wg.Wait() for tunLoop to exit,
	// and on a blocking fd that Close-while-blocked-in-Read race isn't
	// guaranteed to wake the read at all, let alone promptly — so instead of
	// tunLoop noticing the device closed and returning, it (and therefore
	// engine.Stop, and therefore gravinet's own process exit, and therefore
	// daemon(8) and `service gravinet stop`) can hang indefinitely. Read
	// below already retries on EAGAIN/EWOULDBLOCK with a short sleep — that
	// logic only ever actually triggers, and this whole mechanism only
	// works, once the fd is genuinely non-blocking, which is what this call
	// establishes.
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		f.Close()
		return nil, fmt.Errorf("set nonblock: %w", err)
	}

	var ifr ifreqName
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, tunGIFName, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("TUNGIFNAME: %v", e)
	}
	assigned := nullTerminated(ifr.Name[:])

	// Multi-af mode: without this, tun(4) assumes every packet is AF_INET and
	// rejects IPv6, since there's no other way for the kernel to tell them
	// apart on a raw byte stream with no link-layer framing of its own.
	headMode := 1
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, tunSIFHead, uintptr(unsafe.Pointer(&headMode))); e != 0 {
		f.Close()
		destroyIface(assigned)
		return nil, fmt.Errorf("TUNSIFHEAD: %v", e)
	}

	// Out of point-to-point mode: gravinet routes additional redistributed
	// subnets onto this interface (see AddRoute), which wants regular
	// broadcast-style addressing rather than tun(4)'s point-to-point default.
	ifFlags := syscall.IFF_BROADCAST | syscall.IFF_MULTICAST
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, tunSIFMode, uintptr(unsafe.Pointer(&ifFlags))); e != 0 {
		f.Close()
		destroyIface(assigned)
		return nil, fmt.Errorf("TUNSIFMODE: %v", e)
	}

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, tunSIFPid, 0); e != 0 {
		f.Close()
		destroyIface(assigned)
		return nil, fmt.Errorf("TUNSIFPID: %v", e)
	}

	finalName := assigned
	if name != "" && name != assigned {
		if err := renameIface(assigned, name); err != nil {
			// The target name may be held by a stale interface left behind by
			// an earlier disable whose destroy didn't fully land (see
			// destroyIface's comment) — or, for that matter, by anything else
			// that once used this name. Try clearing it and retry once before
			// giving up: this is what makes a later "enable" self-healing
			// instead of requiring a manual config edit and full restart to
			// get a name collision like that to clear.
			if destroyIface(name) {
				err = renameIface(assigned, name)
			}
			if err != nil {
				f.Close()
				destroyIface(assigned)
				return nil, err
			}
		}
		finalName = name
	}

	d := &Device{f: f, name: finalName, mtu: mtu}
	if err := d.setMTU(mtu); err != nil {
		d.Close()
		return nil, err
	}
	// IFF_BROADCAST above gets AddRoute's subnet routing, but it also makes
	// the kernel treat any other address in this interface's /24 (i.e. every
	// other mesh peer's overlay address) as needing ARP resolution before a
	// packet can go out — the same way it would for a real Ethernet NIC.
	// tun(4) has no actual link layer to answer that ARP request with, so it
	// never resolves; the kernel's per-destination hold queue for the
	// unresolved neighbor fills (it's small — a packet or two) almost
	// immediately, and every packet after that gets ENOBUFS synchronously
	// from sendto(), not a timeout — this is what shows up as a plain `ping`
	// to another peer's overlay address failing instantly with "sendto: No
	// buffer space available" on every single packet, first one included.
	// Disabling ARP tells the kernel this interface has no link layer to
	// resolve in the first place, so it hands every on-link packet straight
	// to tun(4)'s output routine instead of queuing it behind a resolution
	// that can never complete.
	if err := d.ifconfig("-arp"); err != nil {
		d.Close()
		return nil, err
	}
	if err := d.Up(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func nullTerminated(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func renameIface(from, to string) error {
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("rename %s: socket: %w", from, err)
	}
	defer syscall.Close(s)

	var newName [ifNameSize]byte
	copy(newName[:], to)
	var ifr ifreqData
	copy(ifr.Name[:], from)
	ifr.Data = uintptr(unsafe.Pointer(&newName[0]))
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), siocSIFName, uintptr(unsafe.Pointer(&ifr)))
	runtime.KeepAlive(newName) // ifr.Data points into it; keep it alive past the syscall
	if e != 0 {
		return fmt.Errorf("rename %s to %s: %v", from, to, e)
	}
	return nil
}

// destroyIface removes a cloned tun interface, retrying briefly on failure.
// Retrying matters: SIOCIFDESTROY is attempted right after closing the
// controlling file descriptor (see Close), and closing that fd releasing the
// interface's last reference kernel-side is not necessarily instantaneous
// relative to the ioctl running — a bare "device busy"-class failure on the
// first attempt, gone by the second, is exactly the kind of transient race
// that used to fail silently here (the ioctl's return value was never
// checked at all), leaving a stale interface behind. A later attempt to
// re-create a network with the same name would then fail its rename step
// (see New) with that name still taken — which is what unreliable "enable"
// while "disable" mostly worked looked like from the outside: the address
// and route were genuinely removed (Close does that explicitly first), so
// traffic did stop, but the interface object itself sometimes didn't
// actually go away.
func destroyIface(name string) bool {
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(s)
	var ifr ifreqName
	copy(ifr.Name[:], name)
	for attempt := 0; attempt < 5; attempt++ {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), siocIFDestroy, uintptr(unsafe.Pointer(&ifr)))
		if e == 0 {
			return true
		}
		if e == syscall.ENXIO {
			return true // already gone - nothing left to do
		}
		if attempt < 4 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return false
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

// AddIPv4 assigns a regular (non-point-to-point) address with a netmask, so
// the kernel creates the connected route for the whole subnet the way it
// does on Linux and Windows — matching the IFF_BROADCAST mode New put this
// interface into.
func (d *Device) AddIPv4(addr netip.Addr, prefixLen int) error {
	mask := net.CIDRMask(prefixLen, 32)
	return d.ifconfig("inet", addr.String(), "netmask", net.IP(mask).String())
}

func (d *Device) AddIPv6(addr netip.Addr, prefixLen int) error {
	return d.ifconfig("inet6", addr.String()+"/"+strconv.Itoa(prefixLen))
}

// AddRoute installs "<prefix> -interface <n>" so the kernel hands matching
// packets to this tun. Best-effort via the route(8) command (untested here).
// FreeBSD route(8) has no per-route metric equivalent to Linux's priority, so
// the metric is accepted for interface parity but not applied here.
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

// Read returns one L3 packet, stripping the 4-byte address-family header
// TUNSIFHEAD mode prepends.
//
// Reuses d.rbuf across calls instead of allocating fresh every time: this is
// the read path an upload speedtest sources its data from (the OS writes the
// outbound HTTP request body into this tun device; Read is how that gets
// picked up, encrypted, and sent out), and allocating-then-discarding a
// multi-KB buffer on every single packet adds real GC pressure right when
// sustained throughput matters most.
//
// Tolerates EINTR/EAGAIN/EWOULDBLOCK rather than surfacing them, since
// mesh.Engine's tunLoop returns permanently the moment Read returns any
// non-nil error (on the assumption that only Close() — an intentional
// shutdown — produces one), so a transient, retryable error surfacing here
// instead would silently and permanently kill outbound packet delivery for
// this network. But retrying EAGAIN with no delay at all, as this used to,
// is a true busy-spin every time the ring is briefly empty — a completely
// ordinary, frequent condition, not a rare edge case — and this is called
// from mesh.Engine's single dedicated per-network goroutine in a tight loop,
// so that spin pegs a CPU core at 100% with nothing productive happening.
// That's the actual explanation for "download always works, upload never
// does": download only ever needs Write (a single syscall, no loop, nothing
// to spin on), while upload depends entirely on this loop — spinning here
// starves the kernel of the scheduling time it needs to actually hand
// outbound TCP segments to the tun driver in the first place, on a
// resource-constrained VM in particular. EINTR retries immediately (a
// signal, not "no data yet," doesn't warrant a delay); EAGAIN/EWOULDBLOCK
// get a brief sleep instead — short enough not to add meaningful latency to
// ordinary mesh traffic, long enough to stop pegging a core.
func (d *Device) Read(p []byte) (int, error) {
	need := len(p) + 4
	if cap(d.rbuf) < need {
		d.rbuf = make([]byte, need)
	}
	buf := d.rbuf[:need]
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				time.Sleep(time.Millisecond)
				continue
			}
			return 0, err
		}
		if n <= 4 {
			return 0, nil
		}
		return copy(p, buf[4:n]), nil
	}
}

// Write prepends the 4-byte address-family header TUNSIFHEAD mode requires.
//
// Retries on EINTR/EAGAIN/EWOULDBLOCK rather than surfacing them, mirroring
// Read's handling just above and for the identical reason: New sets this fd
// non-blocking (so a concurrent Close can interrupt a blocked Read during
// shutdown — see New's comment), and non-blocking mode governs Write just as
// much as Read. Without this, a transient EWOULDBLOCK — the kernel's tun
// receive queue being momentarily full under a burst of inbound traffic,
// exactly what a throughput test's received side produces — looked like a
// real write failure to the caller. deliverInner (internal/mesh/engine.go)
// treats any Write error as "log it and drop this packet," with no retry of
// its own, so an unretried EWOULDBLOCK here silently dropped packets under
// load instead of the packet ever reaching its destination — which is
// exactly the write-side half of the traffic an upload speedtest's received
// data exercises hardest (see the receiving peer's side of the flow this
// package's own tun(4) framing comment above describes).
func (d *Device) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	af := uint32(syscall.AF_INET)
	if p[0]>>4 == 6 {
		af = uint32(syscall.AF_INET6)
	}
	buf := make([]byte, len(p)+4)
	binary.BigEndian.PutUint32(buf[:4], af) // network byte order, per tun(4)
	copy(buf[4:], p)
	for {
		n, err := d.f.Write(buf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				time.Sleep(time.Millisecond)
				continue
			}
			return 0, err
		}
		if n >= 4 {
			n -= 4
		}
		return n, nil
	}
}

func (d *Device) Name() string { return d.name }

// IfIndex returns this TUN's kernel interface index — e.g. for passing as
// DefaultGateway's excludeIfIndex, so a physical-gateway lookup never
// mistakes gravinet's own tunnel-routed default for the real one. See
// gateway_freebsd.go's ifIndexByName.
func (d *Device) IfIndex() (int32, error) {
	return ifIndexByName(d.name)
}
func (d *Device) MTU() int     { return d.mtu }

// Close closes the device file and explicitly destroys the cloned interface.
// Unlike macOS's utun, FreeBSD's tun(4) interfaces aren't documented as
// disappearing from the system just because the controlling file descriptor
// closed, so this doesn't assume that: it follows the file close with an
// explicit SIOCIFDESTROY, the same as the reference implementation
// (wireguard-go's tun_freebsd.go) does, rather than infer cleanup from a
// side effect of an unrelated step — the exact assumption that turned out to
// be wrong on Windows for the equivalent Wintun/netsh case.
//
// If destroyIface's retries are all exhausted, that failure is now returned
// (it used to be silently discarded) rather than the file-close error alone,
// since a lingering interface — not a closed file — is what actually breaks
// a subsequent re-enable of the same network (see destroyIface's comment).
func (d *Device) Close() error {
	err := d.f.Close()
	if !destroyIface(d.name) {
		if err == nil {
			err = fmt.Errorf("destroy interface %s: gave up after retries", d.name)
		}
	}
	return err
}
