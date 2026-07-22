//go:build openbsd

// OpenBSD overlay interface via the built-in tun(4) driver in its default
// layer-3 mode. Compared with tun_freebsd.go this backend is deliberately
// smaller, because OpenBSD's tun(4) needs far less setup:
//
//   - No TUNSIFHEAD: OpenBSD's tun(4) *always* prepends each packet with a
//     4-byte address family, so there is no mode to toggle — the multi-af
//     framing FreeBSD has to opt into with TUNSIFHEAD is just how the device
//     behaves here. (tap(4) is the layer-2 variant; we want tun(4).)
//   - No TUNSIFMODE/TUNSIFPID: not needed.
//   - No SIOCSIFNAME: OpenBSD interface names are driver+unit and can't be
//     renamed. The device is whatever tunN we opened, so unlike the FreeBSD
//     and macOS backends we do NOT honor the requested name — Name() reports
//     the real tunN and every caller (routing, resolver, logging) already
//     keys off Dev.Name() rather than the requested name, so this is a
//     visible-name difference only, not a functional one. See main.go's
//     buildNet: the requested "mesh0" is passed in, but the returned
//     dev.Name() is what flows on to AddRoute/dnssync/etc.
//
// Address/MTU/route configuration shells out to ifconfig(8)/route(8) from the
// base system, exactly like tun_freebsd.go and tun_darwin.go, rather than
// reaching for raw SIOCSIFADDR-family ioctls.
//
// BYTE ORDER — the one thing to verify on real hardware. tun(4)'s 4-byte AF
// prefix is written here in network byte order (big-endian), matching the
// FreeBSD (TUNSIFHEAD) and macOS (utun) backends in this package and the
// wireguard-go OpenBSD backend it was cross-checked against. The Read path
// does NOT depend on this: it strips the 4-byte header and returns the inner
// packet untouched, letting the mesh layer read the real IP header itself, so
// even if a given kernel disagreed about header endianness, receive would
// still work — only the AF the kernel sees on *write* rides on this
// convention. If IPv6 specifically misbehaves on write on some OpenBSD
// release, this constant is the first place to look.
//
// Compiles for openbsd/{amd64,arm64,386,arm,riscv64} but, like the Windows,
// macOS and FreeBSD backends, is not exercised on the Linux build host; the
// ifconfig/route/ioctl paths are marked accordingly.
package tun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// maxTunUnit bounds the /dev/tunN scan in New. OpenBSD ships tun0..tun3 as
// device nodes by default and can clone more via `ifconfig tunN create`; a
// ceiling of 64 is far more networks than anyone runs and keeps a broken
// system (e.g. permissions) from spinning forever.
const maxTunUnit = 64

// Device is an OpenBSD tun(4) interface carrying raw L3 packets.
type Device struct {
	f    *os.File
	name string
	mtu  int
	// rbuf backs Read, reused across calls rather than reallocated per packet
	// — same rationale as tun_freebsd.go's rbuf: this is the read side of the
	// data path and per-packet allocation here is pure GC pressure. Safe
	// without a lock: Read is only ever called from mesh.Engine's single
	// dedicated per-network goroutine.
	rbuf []byte
	// v4Addr/v6Addr remember the address AddIPv4/AddIPv6 assigned. AddRoute
	// needs whichever one matches a route's family as the route's gateway —
	// see AddRoute's comment for why a gateway is required here at all.
	v4Addr netip.Addr
	v6Addr netip.Addr
}

// New opens the first usable tun(4) device. It tries to clone/open tunN for
// ascending N, cloning the interface first (best-effort — on stock OpenBSD
// tun0..3 already exist, and `ifconfig create` is what materializes higher
// units and their /dev nodes) and then opening /dev/tunN. The requested name
// is accepted for signature parity with the other backends but not applied,
// since OpenBSD interfaces can't be renamed (see the package comment).
func New(name string, mtu int) (*Device, error) {
	var (
		f       *os.File
		devName string
		lastErr error
	)
	for unit := 0; unit < maxTunUnit; unit++ {
		ifn := "tun" + strconv.Itoa(unit)
		// Best-effort clone; ignores "already exists" and lack of privilege —
		// the open below is the real gate. Creating an already-present tunN
		// is harmless.
		_ = exec.Command("ifconfig", ifn, "create").Run()

		dev := "/dev/" + ifn
		fh, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			lastErr = err
			continue // busy (another gravinet net / other user) or no node — try next unit
		}
		f, devName = fh, ifn
		break
	}
	if f == nil {
		if lastErr == nil {
			lastErr = errors.New("no free tun device")
		}
		return nil, fmt.Errorf("open tun device: %w", lastErr)
	}

	// Non-blocking so a concurrent Close() actually interrupts a Read() in
	// progress during shutdown — identical mechanism and rationale to
	// tun_freebsd.go and tun_linux.go: mesh.Engine.Stop() closes the device
	// to unblock tunLoop's dev.Read(); on a blocking fd that race can hang
	// process exit (and `rcctl stop gravinet`) indefinitely. Read/Write below
	// retry EAGAIN/EWOULDBLOCK with a short sleep, which only works once the
	// fd is genuinely non-blocking.
	if err := syscall.SetNonblock(int(f.Fd()), true); err != nil {
		f.Close()
		destroyIface(devName)
		return nil, fmt.Errorf("set nonblock: %w", err)
	}

	d := &Device{f: f, name: devName, mtu: mtu}
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

// AddIPv4 assigns an address with a netmask via ifconfig(8) (same `inet ADDR
// netmask MASK` form FreeBSD accepts). Unlike FreeBSD/Linux/macOS, this alone
// does NOT reliably give the kernel a connected route for the whole subnet:
// OpenBSD's tun(4) defaults to point-to-point mode (see the package comment)
// and has no equivalent of FreeBSD's TUNSIFMODE ioctl to take it out of that
// mode, so assigning "netmask" here typically yields only the usual
// automatic host-local route for addr itself. The caller (mesh.Engine's
// assignAddr) already knows not to rely on this and explicitly installs the
// connected route via AddRoute right afterward — the same fix already in
// place for a matching macOS report; see addressing.go's assignAddr comment.
func (d *Device) AddIPv4(addr netip.Addr, prefixLen int) error {
	mask := net.CIDRMask(prefixLen, 32)
	if err := d.ifconfig("inet", addr.String(), "netmask", net.IP(mask).String()); err != nil {
		return err
	}
	d.v4Addr = addr
	return nil
}

func (d *Device) AddIPv6(addr netip.Addr, prefixLen int) error {
	if err := d.ifconfig("inet6", addr.String()+"/"+strconv.Itoa(prefixLen)); err != nil {
		return err
	}
	d.v6Addr = addr
	return nil
}

// AddRoute installs a static route for prefix p pointing at this tun so the
// kernel hands matching packets to it — for both this network's own connected
// subnet (installed explicitly by mesh.Engine's assignAddr, since AddIPv4
// can't be relied on for that here; see its comment) and every redistributed
// prefix another mesh node advertises (via syncRoute).
//
// Route(8) syntax note: earlier versions of this file used FreeBSD's
// "-interface tunN" modifier, which doesn't exist on OpenBSD's route(8) (it
// has "-iface", a boolean flag with different meaning, and "-ifp ifname" for
// picking an interface — see route(8)'s BUGS section for known "-ifp"
// misbehavior when no default route is present). The portable fix, used by
// tun-based VPNs on OpenBSD generally (OpenVPN's "topology subnet" and
// wireguard-go both do this — the latter is why multiple peers can share one
// tun despite tun(4) defaulting to point-to-point, see the package comment),
// is to give route(8) this device's own address as the "gateway": the kernel
// already has that address down as directly attached to this interface (an
// automatic side effect of any address assignment, point-to-point or not),
// so it resolves through tun and never touches the network.
func (d *Device) AddRoute(p netip.Prefix, metric int) error {
	_ = metric // no OpenBSD route(8) equivalent to a per-route metric; same as tun_freebsd.go
	fam, destFlag, dest, gw, err := d.routeFamily(p)
	if err != nil {
		return err
	}
	out, cerr := exec.Command("route", "-n", "add", fam, destFlag, dest, gw).CombinedOutput()
	if cerr != nil {
		return fmt.Errorf("route add %s dev %s: %v (%s)", p, d.name, cerr, out)
	}
	return nil
}

// DelRoute removes the route for prefix on this interface. Passing the same
// gateway used in AddRoute isn't strictly required by route(8) but pins down
// exactly which route to remove, mirroring how it was installed.
func (d *Device) DelRoute(p netip.Prefix, metric int) error {
	_ = metric
	fam, destFlag, dest, gw, err := d.routeFamily(p)
	if err != nil {
		return err
	}
	out, cerr := exec.Command("route", "-n", "delete", fam, destFlag, dest, gw).CombinedOutput()
	if cerr != nil {
		return fmt.Errorf("route delete %s dev %s: %v (%s)", p, d.name, cerr, out)
	}
	return nil
}

// routeFamily returns the route(8) family flag, the destination modifier and
// notation, and the gateway address to pair with prefix p (this device's own
// v4 or v6 address, whichever matches p's family). Errors if that family's
// address hasn't been assigned yet — AddIPv4/AddIPv6 must run before any
// route referencing that family.
//
// destFlag/dest split host vs network destinations explicitly rather than
// always using "-net" with a full-length ("/32" or "/128") CIDR suffix, which
// is what this file did previously. That's defensible reading route(8)'s
// address-notation rules in isolation (a "/XX" suffix alone already implies
// a network destination, redundantly with -net), but it is not the idiom
// route(8)'s own manual or any real single-address example actually uses —
// every one of them (see gre(4)'s "route add -host 192.0.2.2 ... -ifp mgreN"
// and the analogous AMPRNet gif(4) example) pairs -host with a bare address,
// never -net with an all-ones mask. A live report matched that seam exactly:
// redistributed /32 mesh routes went missing from the OpenBSD routing table
// while every other prefix length installed fine — the one shape -net+/32
// never gets exercised by anything else in this file. -host with the bare
// address (no "/32" suffix at all — a suffix is specifically what makes
// route(8) treat a destination as a network, so pairing one with -host would
// be self-contradictory) is the unambiguous fix for a full-length prefix.
func (d *Device) routeFamily(p netip.Prefix) (fam, destFlag, dest, gw string, err error) {
	host := (p.Addr().Is6() && p.Bits() == 128) || (!p.Addr().Is6() && p.Bits() == 32)
	destFlag, dest = "-net", p.String()
	if host {
		destFlag, dest = "-host", p.Addr().String()
	}
	if p.Addr().Is6() {
		if !d.v6Addr.IsValid() {
			return "", "", "", "", fmt.Errorf("route %s: no IPv6 address assigned yet on %s (AddIPv6 must run first)", p, d.name)
		}
		return "-inet6", destFlag, dest, d.v6Addr.String(), nil
	}
	if !d.v4Addr.IsValid() {
		return "", "", "", "", fmt.Errorf("route %s: no IPv4 address assigned yet on %s (AddIPv4 must run first)", p, d.name)
	}
	return "-inet", destFlag, dest, d.v4Addr.String(), nil
}

// Read returns one L3 packet, stripping the 4-byte address-family header
// tun(4) prepends. Deliberately does not interpret that header (see the
// package comment on byte order): the inner IP packet is self-describing, so
// stripping four bytes is all that's needed and it's endianness-independent.
//
// Reuses d.rbuf and tolerates EINTR/EAGAIN/EWOULDBLOCK with a short sleep on
// EAGAIN, for the identical reasons spelled out at length in
// tun_freebsd.go's Read: surfacing a transient error would permanently kill
// this network's outbound delivery (tunLoop returns for good on any error),
// while busy-spinning on EAGAIN pegs a core on an ordinary empty-ring
// condition.
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

// Write prepends the 4-byte address-family header tun(4) requires, derived
// from the packet's own IP version nibble. See the package comment for the
// network-byte-order assumption. Retries EINTR/EAGAIN/EWOULDBLOCK exactly as
// tun_freebsd.go's Write does: the fd is non-blocking (so Close can interrupt
// Read), and an unretried EWOULDBLOCK under an inbound burst would look like
// a write failure and silently drop the packet (deliverInner has no retry of
// its own).
func (d *Device) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	af := uint32(syscall.AF_INET)
	if p[0]>>4 == 6 {
		af = uint32(syscall.AF_INET6)
	}
	buf := make([]byte, len(p)+4)
	binary.BigEndian.PutUint32(buf[:4], af) // network byte order; see package comment
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
// gateway_openbsd.go's ifIndexByName.
func (d *Device) IfIndex() (int32, error) {
	return ifIndexByName(d.name)
}
func (d *Device) MTU() int     { return d.mtu }

// Close closes the device file and destroys the cloned interface. Closing the
// fd drops the tun's data channel; the explicit destroy then removes the
// interface (and the addresses/routes on it) so a later re-enable of the same
// network starts clean, mirroring the explicit-teardown choice tun_freebsd.go
// made rather than assuming the interface disappears on its own. Best-effort
// on the shutdown path: a destroy failure is folded into the returned error
// only if closing the file didn't already fail.
func (d *Device) Close() error {
	err := d.f.Close()
	if !destroyIface(d.name) && err == nil {
		err = fmt.Errorf("destroy interface %s: gave up after retries", d.name)
	}
	return err
}

// destroyIface removes a tun interface via `ifconfig tunN destroy`, retrying
// briefly. Like the FreeBSD SIOCIFDESTROY path, releasing the interface right
// after closing the controlling fd can momentarily race the kernel dropping
// its last reference, so a transient failure that's gone a moment later is
// expected; an already-absent interface counts as success.
func destroyIface(name string) bool {
	for attempt := 0; attempt < 5; attempt++ {
		out, err := exec.Command("ifconfig", name, "destroy").CombinedOutput()
		if err == nil {
			return true
		}
		// "interface does not exist" — already gone, nothing to do.
		if isNoSuchIface(out) {
			return true
		}
		if attempt < 4 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return false
}

func isNoSuchIface(ifconfigOut []byte) bool {
	s := string(ifconfigOut)
	return containsFold(s, "does not exist") || containsFold(s, "no such")
}

// containsFold is a tiny case-insensitive substring check; avoids pulling in
// strings just for one call and keeps the ASCII-only ifconfig error matching
// obvious.
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			t := sub[j]
			if t >= 'A' && t <= 'Z' {
				t += 'a' - 'A'
			}
			if c != t {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
