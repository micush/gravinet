//go:build windows

// Windows overlay interface via the Wintun userspace TUN driver. Windows has no
// built-in TUN, so this backend uses Wintun (the WireGuard project's signed
// kernel driver) through syscall — no cgo, no golang.org/x/sys — and its
// ring-buffer session API for the data path. Address/MTU configuration uses
// netsh, which ships with Windows.
//
// The Wintun DLL is embedded in the binary at build time and extracted to a
// per-user cache directory on first use, so a release build is a single
// self-contained .exe with no loose files. If no real DLL was bundled (the
// source tree ships a placeholder), the backend instead loads a wintun.dll
// placed beside the executable. Either way the signed kernel driver itself is
// still required at runtime — Windows cannot create a TUN device without one.
// This compiles for windows/amd64 and windows/arm64 but is not exercised on the
// Linux build host.
package tun

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	wintun                 *syscall.LazyDLL
	procCreateAdapter      *syscall.LazyProc
	procCloseAdapter       *syscall.LazyProc
	procStartSession       *syscall.LazyProc
	procEndSession         *syscall.LazyProc
	procGetReadWaitEvent   *syscall.LazyProc
	procReceivePacket      *syscall.LazyProc
	procReleaseReceive     *syscall.LazyProc
	procAllocateSendPacket *syscall.LazyProc
	procSendPacket         *syscall.LazyProc
	procGetAdapterLUID     *syscall.LazyProc

	wintunOnce sync.Once
	wintunErr  error

	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procWaitForSingleObj = kernel32.NewProc("WaitForSingleObject")
)

// ensureWintun materializes the Wintun driver DLL and resolves its entry points
// exactly once. The DLL is embedded in the binary when a real one was bundled at
// build time; otherwise it is loaded from beside the executable.
func ensureWintun() error {
	wintunOnce.Do(func() {
		path, err := materializeWintun()
		if err != nil {
			wintunErr = fmt.Errorf("wintun: prepare driver: %w", err)
			return
		}
		wintun = syscall.NewLazyDLL(path)
		if err := wintun.Load(); err != nil {
			wintunErr = fmt.Errorf("wintun: load %q (Wintun driver unavailable): %w", path, err)
			return
		}
		procCreateAdapter = wintun.NewProc("WintunCreateAdapter")
		procCloseAdapter = wintun.NewProc("WintunCloseAdapter")
		procStartSession = wintun.NewProc("WintunStartSession")
		procEndSession = wintun.NewProc("WintunEndSession")
		procGetReadWaitEvent = wintun.NewProc("WintunGetReadWaitEvent")
		procReceivePacket = wintun.NewProc("WintunReceivePacket")
		procReleaseReceive = wintun.NewProc("WintunReleaseReceivePacket")
		procAllocateSendPacket = wintun.NewProc("WintunAllocateSendPacket")
		procSendPacket = wintun.NewProc("WintunSendPacket")
		procGetAdapterLUID = wintun.NewProc("WintunGetAdapterLUID")
	})
	return wintunErr
}

// materializeWintun returns a path to a loadable wintun.dll. If a real driver
// was embedded at build time (PE "MZ" magic), it is written to a per-user cache
// directory (keyed by content hash so upgrades don't collide) and that path is
// returned. Otherwise it returns the bare name, so the OS loader finds a
// wintun.dll shipped alongside the binary — preserving the side-by-side mode.
func materializeWintun() (string, error) {
	if len(wintunDLLBytes) < 2 || wintunDLLBytes[0] != 'M' || wintunDLLBytes[1] != 'Z' {
		return "wintun.dll", nil // placeholder embedded → fall back to side-by-side
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "gravinet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(wintunDLLBytes)
	path := filepath.Join(dir, fmt.Sprintf("wintun-%x.dll", sum[:8]))
	if fi, err := os.Stat(path); err != nil || fi.Size() != int64(len(wintunDLLBytes)) {
		tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
		if err := os.WriteFile(tmp, wintunDLLBytes, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	return path, nil
}

const (
	ringCapacity    = 0x400000 // 4 MiB ring
	errNoMoreItems  = 259
	waitPollMs      = 1000 // bounded re-check interval; see the comment in Read
	maxWintunPacket = 0xFFFF
)

// Device is a Windows Wintun interface carrying raw L3 packets.
type Device struct {
	adapter uintptr
	session uintptr
	event   uintptr
	name    string
	mtu     int
	closing atomic.Bool // set once by Close; see Read/Write/Close below
	// io guards Read/Write against Close: they hold it for read (so multiple
	// calls, though in practice there's only ever one reader, aren't mutually
	// exclusive with each other), Close takes it for write. See Close's
	// comment for why this matters.
	io sync.RWMutex

	// addr4/addr6 record what AddIPv4/AddIPv6 assigned, so Close can
	// explicitly remove them (and, with them, netsh's implicit connected
	// route for that subnet) before destroying the adapter. See Close's
	// comment for why this isn't just belt-and-suspenders.
	addr4, addr6 netip.Prefix

	// routes records every prefix AddRoute has added on this interface (the
	// self-address subnet route from addressing.go, its sleep/resume re-add
	// in control.go, and any redistributed route from routes.go), so Close
	// can remove each one explicitly for the same reason it explicitly
	// removes addr4/addr6: adapter destruction was already shown not to be a
	// reliable proxy for "every route through this interface is gone" for
	// the address's own connected route, and netsh's separately-added static
	// routes (store=active) are, if anything, even less tied to the
	// adapter's lifecycle than that connected route was. routesMu guards it
	// against AddRoute/DelRoute being called concurrently with Close from
	// another goroutine (route redistribution isn't necessarily quiesced
	// before a network is torn down).
	routesMu sync.Mutex
	routes   map[netip.Prefix]struct{}
}

// New creates a Wintun adapter and starts a session.
func New(name string, mtu int) (*Device, error) {
	if err := ensureWintun(); err != nil {
		return nil, err
	}
	if name == "" {
		name = "gravinet"
	}
	namePtr, _ := syscall.UTF16PtrFromString(name)
	typePtr, _ := syscall.UTF16PtrFromString("gravinet")

	adapter, _, err := procCreateAdapter.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(typePtr)),
		0, // requested GUID: let Wintun choose
	)
	if adapter == 0 {
		return nil, fmt.Errorf("WintunCreateAdapter (is wintun.dll present?): %v", err)
	}
	session, _, err := procStartSession.Call(adapter, uintptr(ringCapacity))
	if session == 0 {
		procCloseAdapter.Call(adapter)
		return nil, fmt.Errorf("WintunStartSession: %v", err)
	}
	event, _, _ := procGetReadWaitEvent.Call(session)

	d := &Device{adapter: adapter, session: session, event: event, name: name, mtu: mtu}
	if err := d.setMTU(mtu); err != nil {
		// Non-fatal: log-worthy but the interface still works at default MTU.
		_ = err
	}
	return d, nil
}

func (d *Device) netsh(args ...string) error {
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %v: %v (%s)", args, err, out)
	}
	return nil
}

func (d *Device) setMTU(mtu int) error {
	return d.netsh("interface", "ipv4", "set", "subinterface", d.name, "mtu="+strconv.Itoa(mtu), "store=persistent")
}

func (d *Device) AddIPv4(addr netip.Addr, prefixLen int) error {
	mask := netip.PrefixFrom(addr, prefixLen)
	_ = mask
	if err := d.netsh("interface", "ipv4", "set", "address",
		"name="+d.name, "static", addr.String(), prefixToMask(prefixLen)); err != nil {
		return err
	}
	d.addr4 = netip.PrefixFrom(addr, prefixLen)
	return nil
}

func (d *Device) AddIPv6(addr netip.Addr, prefixLen int) error {
	if err := d.netsh("interface", "ipv6", "set", "address",
		"interface="+d.name, addr.String()+"/"+strconv.Itoa(prefixLen)); err != nil {
		return err
	}
	d.addr6 = netip.PrefixFrom(addr, prefixLen)
	return nil
}

// AddRoute installs a route for prefix via this interface with the given metric.
// Best-effort via netsh (untested here). Uses "store=active" so it doesn't
// persist across reboots.
func (d *Device) AddRoute(p netip.Prefix, metric int) error {
	v := "ipv4"
	if p.Addr().Is6() {
		v = "ipv6"
	}
	args := []string{"interface", v, "add", "route", "prefix=" + p.String(),
		"interface=" + d.name, "store=active"}
	if metric > 0 {
		args = append(args, "metric="+strconv.Itoa(metric))
	}
	if err := d.netsh(args...); err != nil {
		return err
	}
	d.routesMu.Lock()
	if d.routes == nil {
		d.routes = map[netip.Prefix]struct{}{}
	}
	d.routes[p] = struct{}{}
	d.routesMu.Unlock()
	return nil
}

// DelRoute removes the route for prefix on this interface.
func (d *Device) DelRoute(p netip.Prefix, metric int) error {
	_ = metric // netsh delete route matches on prefix+interface
	v := "ipv4"
	if p.Addr().Is6() {
		v = "ipv6"
	}
	err := d.netsh("interface", v, "delete", "route", "prefix="+p.String(),
		"interface="+d.name, "store=active")
	if err == nil {
		d.routesMu.Lock()
		delete(d.routes, p)
		d.routesMu.Unlock()
	}
	return err
}

func (d *Device) Up() error { return nil } // Wintun adapters are up once a session starts

// Read returns one L3 packet, blocking on the read-wait event when the ring is
// empty.
//
// Close is expected to make a blocked Read return promptly: WintunEndSession
// signals this same event so a waiter wakes up. But the original version of
// this code raced Close's zeroing of d.session against Read's check of it —
// two goroutines touching a plain uintptr field with no synchronization — so
// a Read that woke up a hair before that write landed would loop back around
// into another WaitForSingleObject that nothing was going to signal again,
// hanging forever. That hang is what was blocking network deletion: removing
// a network waits (mesh.Engine.RemoveNetwork's ns.wg.Wait()) for exactly this
// loop to exit after Close is called.
//
// Fixed two ways: closing is an atomic.Bool set by Close before it touches
// anything else, checked here instead of the racy session field; and the
// wait itself is bounded (waitPollMs) rather than infinite, so even if the
// session-end signal were somehow missed, Read re-checks closing at worst
// once a second instead of blocking forever.
//
// Holds io for read for its whole duration - see Close's comment for why.
func (d *Device) Read(p []byte) (int, error) {
	d.io.RLock()
	defer d.io.RUnlock()
	for {
		if d.closing.Load() {
			return 0, fmt.Errorf("tun: session closed")
		}
		var size uint32
		pkt, _, _ := procReceivePacket.Call(d.session, uintptr(unsafe.Pointer(&size)))
		if pkt != 0 {
			n := copy(p, unsafe.Slice((*byte)(unsafe.Pointer(pkt)), size))
			procReleaseReceive.Call(d.session, pkt)
			return n, nil
		}
		// Empty ring: wait for the next packet, the session ending, or the
		// poll interval — whichever comes first — then loop back to the
		// closing check above.
		procWaitForSingleObj.Call(d.event, uintptr(waitPollMs))
	}
}

// Write sends one L3 packet through the ring.
func (d *Device) Write(p []byte) (int, error) {
	d.io.RLock()
	defer d.io.RUnlock()
	if d.closing.Load() {
		return 0, fmt.Errorf("tun: session closed")
	}
	if len(p) == 0 || len(p) > maxWintunPacket {
		return 0, nil
	}
	pkt, _, err := procAllocateSendPacket.Call(d.session, uintptr(len(p)))
	if pkt == 0 {
		return 0, fmt.Errorf("WintunAllocateSendPacket: %v", err)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(pkt)), len(p)), p)
	procSendPacket.Call(d.session, pkt)
	return len(p), nil
}

func (d *Device) Name() string { return d.name }

// IfIndex returns this Wintun adapter's current kernel interface index — for
// passing as DefaultGateway's excludeIfIndex, and for AddGatewayRoute's own
// use resolving *other* interfaces (see gateway_windows.go). Wintun's own
// API only exposes a LUID (WintunGetAdapterLUID) — a stable identifier that
// survives adapter enable/disable, unlike the index — so getting the index
// is a second call, ConvertInterfaceLuidToIndex (netioapi.h/iphlpapi.dll),
// converting the LUID gateway_windows.go's own route calls don't otherwise
// need. Both are cheap, synchronous calls; no reason to cache the result
// given callers already only reach this rarely (once per full-tunnel
// activation, roam, or seed-sync tick).
func (d *Device) IfIndex() (int32, error) {
	if err := ensureWintun(); err != nil {
		return 0, err
	}
	var luid uint64
	// WintunGetAdapterLUID has no return value (void) — it always succeeds
	// for a live adapter handle.
	procGetAdapterLUID.Call(d.adapter, uintptr(unsafe.Pointer(&luid)))
	idx, err := convertInterfaceLUIDToIndex(luid)
	if err != nil {
		return 0, fmt.Errorf("resolve this adapter's interface index: %w", err)
	}
	return idx, nil
}

func (d *Device) MTU() int     { return d.mtu }

// Close ends the Wintun session and closes the adapter. Idempotent (safe to
// call more than once) since it's reached from both a live "remove this
// network" path and the whole-daemon shutdown path, and either could call it.
//
// This used to just be WintunEndSession followed by WintunCloseAdapter, on
// the assumption that destroying the adapter implicitly tears down whatever
// netsh set up on it — including the "connected" route netsh's own "set
// address" implicitly creates for the assigned subnet, which nothing here
// ever removes explicitly. That assumption was the bug: gravinet's own
// bookkeeping (config, engine, sessions) all correctly agreed a disabled
// network was gone, confirmed by an explicit post-removal check against the
// live engine state — and it still kept passing traffic, round-tripped
// through a real remote peer, not some local artifact. Adapter destruction
// evidently isn't a reliable enough proxy for "the route to this subnet is
// gone" to depend on. So the address (and with it, that connected route) is
// now removed explicitly, first, before touching the session or the adapter
// at all — not inferred from a side effect of a later step.
//
// That fix covered the interface's own address and its implicit connected
// route, but not any route added via AddRoute — the self-address subnet
// route addressing.go adds (and control.go re-adds after sleep/resume), or
// any redistributed route from routes.go. Those are netsh "store=active"
// static routes, bookkept even more separately from the adapter's lifecycle
// than the connected route was, so the same fix applies to them for the same
// reason: remove them explicitly, first, rather than trust adapter
// destruction to take them with it. This is why AddRoute/DelRoute maintain
// the routes map — Close needs to know what to remove without depending on
// every caller that ever added a route to also remember to remove it.
//
// The synchronization below (io.Lock before touching the adapter) is a
// separate, still-valid fix for a different problem: Read is a separate
// goroutine (mesh.Engine's tunLoop) that can be blocked inside Wintun's own
// receive/wait call at the exact moment Close runs, and tearing down the
// adapter while that's in flight is exactly the kind of concurrent
// native-API misuse that can go wrong silently. Taking io for write here,
// after ending the session but before touching the adapter, can only succeed
// once every Read/Write currently in flight has returned (they hold it for
// read), and once Close holds it, nothing new can start (they'd see closing
// and bail before doing anything with the adapter).
func (d *Device) Close() error {
	if !d.closing.CompareAndSwap(false, true) {
		return nil // already closed
	}
	// Explicit, best-effort: this is a shutdown path, and a failure here
	// (e.g. the interface already gone some other way) shouldn't block the
	// rest of teardown. Errors aren't surfaced anywhere yet — there's no
	// logger in this package (see the file header) — but a future caller
	// could check them if that changes.
	d.routesMu.Lock()
	routes := make([]netip.Prefix, 0, len(d.routes))
	for p := range d.routes {
		routes = append(routes, p)
	}
	d.routes = nil
	d.routesMu.Unlock()
	// Concurrent, not sequential: each is a real netsh.exe subprocess spawn,
	// and this whole method runs synchronously inside the HTTP request that
	// disabled the network (mesh.Engine.RemoveNetwork -> mutateConfig's
	// reload, still holding the response open) — a handful of these run one
	// at a time could add up to real, user-visible request latency on
	// Windows, where process spawning is measurably slower than Linux/macOS
	// to begin with. They target disjoint routes on the same interface, so
	// there's no ordering dependency between them.
	var wg sync.WaitGroup
	for _, p := range routes {
		wg.Add(1)
		go func(p netip.Prefix) {
			defer wg.Done()
			v := "ipv4"
			if p.Addr().Is6() {
				v = "ipv6"
			}
			d.netsh("interface", v, "delete", "route", "prefix="+p.String(),
				"interface="+d.name, "store=active")
		}(p)
	}
	wg.Wait()
	if d.addr4.IsValid() {
		d.netsh("interface", "ipv4", "delete", "address", "name="+d.name, "addr="+d.addr4.Addr().String())
	}
	if d.addr6.IsValid() {
		d.netsh("interface", "ipv6", "delete", "address", "interface="+d.name, "address="+d.addr6.Addr().String())
	}
	if d.session != 0 {
		procEndSession.Call(d.session)
	}
	d.io.Lock()
	defer d.io.Unlock()
	if d.adapter != 0 {
		procCloseAdapter.Call(d.adapter)
	}
	return nil
}

func prefixToMask(prefixLen int) string {
	var m uint32
	if prefixLen > 0 {
		m = ^uint32(0) << (32 - prefixLen)
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
}
