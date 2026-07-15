// Package transport is gravinet's UDP underlay. It binds IPv4 and/or IPv6
// sockets, walking a primary-then-fallback port list, and runs a worker pool
// (cores-1 by default) that reads datagrams into pooled buffers and dispatches
// them to a handler with minimal allocation for low latency. Outbound sends
// round-robin across the same REUSEPORT socket set the read workers use (see
// Send), so writes get the same per-socket spread reads already had instead
// of funneling through a single socket regardless of worker count.
package transport

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"gravinet/internal/logx"
	"gravinet/internal/protocol"
)

// Family distinguishes the underlay address family of a socket/datagram.
type Family uint8

const (
	V4 Family = 4
	V6 Family = 6
)

// Handler receives one inbound datagram. The payload slice is owned by the
// transport and reused after Handler returns — copy anything you retain.
type Handler func(payload []byte, from netip.AddrPort, fam Family)

// Options configure a Transport.
// defaultSocketBuffer is the per-socket SO_RCVBUF/SO_SNDBUF target. The kernel
// default (~208 KB) overflows at multi-Gbps and drops datagrams (RcvbufErrors),
// which TCP sees as loss; 4 MiB gives enough slack to absorb scheduling bursts.
const defaultSocketBuffer = 4 << 20

type Options struct {
	BindAddr      string // wildcard if empty ("" => 0.0.0.0 / ::)
	PrimaryPort   int
	FallbackPorts []int
	ExtraPorts    []int // additional ports bound *concurrently* with the primary (inbound only)
	EnableV4      bool
	EnableV6      bool
	Workers       int // total reader goroutines; >=1
	SocketBuffer  int // SO_RCVBUF/SO_SNDBUF target in bytes; 0 => default
	Handler       Handler
	Log           *logx.Logger
}

// Transport owns the bound sockets and the worker pool.
type Transport struct {
	port    int
	workers int
	handler Handler
	log     *logx.Logger

	conns4 []*net.UDPConn
	conns6 []*net.UDPConn

	// txRR4/txRR6 round-robin the outbound socket within conns4/conns6
	// respectively — see Send. Reads already get real per-core parallelism,
	// one REUSEPORT socket per worker (startWorkers); writes used to always
	// go out conns4[0]/conns6[0], leaving every other bound socket idle on
	// the send side. Every socket in a REUSEPORT set is bound to the
	// identical address:port, so which one actually performs the write
	// changes nothing on the wire (the source port is the same either way)
	// — this only spreads outbound syscalls across sockets instead of
	// piling every write from every goroutine onto a single one. A plain
	// atomic counter (not per-CPU/goroutine-local) is enough here: the
	// increment itself costs a few nanoseconds, dwarfed by the syscall it's
	// selecting a socket for. len(conns4)==1 (reuseport disabled, or a
	// single-worker setup) makes this a no-op — always index 0.
	txRR4 atomic.Uint64
	txRR6 atomic.Uint64

	// extra holds sockets on additional listen ports (config extra_listen_ports).
	// We read from them but never originate from them by default; replies are
	// routed back out the arrival socket via sendFrom so a peer that dialed
	// :443 sees the answer come from :443 (stateful firewalls/NAT require this).
	extra    []famConn
	hasExtra bool

	sendMu   sync.RWMutex
	sendFrom map[netip.AddrPort]*net.UDPConn // remote -> socket to reply from

	pool   sync.Pool
	wg     sync.WaitGroup
	closed atomic.Bool

	rxPackets atomic.Uint64
	txPackets atomic.Uint64
}

// famConn pairs a socket with its address family.
type famConn struct {
	c   *net.UDPConn
	fam Family
}

// maxSendFrom caps the reply-routing map so a flood of spoofed sources on an
// extra port can't grow it without bound; past the cap, new remotes simply use
// the default (primary) send path.
const maxSendFrom = 8192

// binder abstracts socket creation so the port-fallback logic is unit-testable.
type binder func(network, addr string) (*net.UDPConn, error)

func realBinder(network, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: control}
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		return nil, err
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("not a UDP socket")
	}
	return uc, nil
}

// Open binds sockets and starts the worker pool.
func Open(o Options) (*Transport, error) {
	return openWith(o, realBinder)
}

func openWith(o Options, bind binder) (*Transport, error) {
	if !o.EnableV4 && !o.EnableV6 {
		return nil, fmt.Errorf("transport: no address family enabled")
	}
	if o.Workers < 1 {
		o.Workers = 1
	}
	if o.SocketBuffer <= 0 {
		o.SocketBuffer = defaultSocketBuffer
	}
	if o.Handler == nil {
		return nil, fmt.Errorf("transport: nil handler")
	}
	log := o.Log
	if log == nil {
		log = logx.Default()
	}
	o.Log = log // bindGroup logs best-effort fallbacks through o.Log

	socketsPerFamily := 1
	if reusePort {
		socketsPerFamily = o.Workers
	}

	candidates := append([]int{o.PrimaryPort}, o.FallbackPorts...)
	port, conns4, conns6, err := bindGroup(bind, candidates, o, socketsPerFamily)
	if err != nil {
		return nil, err
	}

	t := &Transport{
		port:    port,
		workers: o.Workers,
		handler: o.Handler,
		log:     log,
		conns4:  conns4,
		conns6:  conns6,
	}
	t.pool.New = func() any { b := make([]byte, protocol.MaxUDPPayload); return &b }

	// Bind any configured extra listen ports concurrently. These are best-effort:
	// a privileged or already-taken port (443/53/etc.) is logged and skipped
	// rather than failing startup. One socket per family each keeps it lean.
	host4 := o.BindAddr
	host6 := o.BindAddr
	if host6 == "" {
		host6 = "::"
	}
	seenPort := map[int]bool{port: true}
	for _, ep := range o.ExtraPorts {
		if ep <= 0 || ep > 65535 || seenPort[ep] {
			continue
		}
		seenPort[ep] = true
		_, e4, e6, err := tryBindAll(bind, ep, host4, host6, o, 1, false)
		if err != nil {
			log.Warnf("transport: extra listen port %d not bound (%v) — skipping", ep, err)
			continue
		}
		for _, c := range e4 {
			t.extra = append(t.extra, famConn{c, V4})
		}
		for _, c := range e6 {
			t.extra = append(t.extra, famConn{c, V6})
		}
		log.Infof("transport: also listening on udp port %d (v4=%d, v6=%d)", ep, len(e4), len(e6))
	}
	if len(t.extra) > 0 {
		t.hasExtra = true
		t.sendFrom = make(map[netip.AddrPort]*net.UDPConn)
	}

	t.startWorkers()
	log.Infof("transport: listening on udp port %d (v4=%d socket(s), v6=%d socket(s), workers=%d, reuseport=%v)",
		port, len(conns4), len(conns6), o.Workers, reusePort)
	return t, nil
}

// bindGroup finds a port for the sockets. It first tries strictly — a single
// port where every enabled family binds — and, failing that, falls back to a
// best-effort bind that accepts whatever families come up (e.g. IPv4-only on a
// host without IPv6) as long as at least one does. Partial binds are rolled
// back before trying the next candidate.
func bindGroup(bind binder, candidates []int, o Options, perFamily int) (int, []*net.UDPConn, []*net.UDPConn, error) {
	host4 := o.BindAddr
	host6 := o.BindAddr
	if host6 == "" {
		host6 = "::"
	}

	var lastErr error
	// Pass 1: strict (one port, all enabled families).
	for _, cand := range candidates {
		if cand < 0 || cand > 65535 {
			continue
		}
		port, c4, c6, err := tryBindAll(bind, cand, host4, host6, o, perFamily, true)
		if err == nil {
			return port, c4, c6, nil
		}
		lastErr = err
	}

	// Pass 2: best-effort (accept a partial bind).
	for _, cand := range candidates {
		if cand < 0 || cand > 65535 {
			continue
		}
		port, c4, c6, err := tryBindAll(bind, cand, host4, host6, o, perFamily, false)
		if err == nil {
			if o.EnableV4 && len(c4) == 0 && o.Log != nil {
				o.Log.Warnf("transport: IPv4 could not bind; running IPv6-only")
			}
			if o.EnableV6 && len(c6) == 0 && o.Log != nil {
				o.Log.Warnf("transport: IPv6 could not bind; running IPv4-only")
			}
			return port, c4, c6, nil
		}
		lastErr = err
	}

	return 0, nil, nil, fmt.Errorf("transport: could not bind any candidate port: %w", lastErr)
}

func tryBindAll(bind binder, cand int, host4, host6 string, o Options, perFamily int, strict bool) (int, []*net.UDPConn, []*net.UDPConn, error) {
	var c4, c6 []*net.UDPConn
	rollback := func() {
		for _, c := range c4 {
			c.Close()
		}
		for _, c := range c6 {
			c.Close()
		}
	}

	port := cand // may be refined to a concrete port after the first bind

	bindN := func(network, host string, dst *[]*net.UDPConn) error {
		for i := 0; i < perFamily; i++ {
			addr := net.JoinHostPort(host, fmt.Sprint(port))
			uc, err := bind(network, addr)
			if err != nil {
				return err
			}
			if la, ok := uc.LocalAddr().(*net.UDPAddr); ok && la.Port != 0 {
				port = la.Port // lock subsequent binds to the real port
			}
			setSocketBuffers(uc, o.SocketBuffer, o.Log)
			*dst = append(*dst, uc)
		}
		return nil
	}

	var lastErr error
	if o.EnableV4 {
		if err := bindN("udp4", host4, &c4); err != nil {
			if strict {
				rollback()
				return 0, nil, nil, err
			}
			for _, c := range c4 { // tolerate: drop this family's partial sockets
				c.Close()
			}
			c4 = nil
			lastErr = err
		}
	}
	if o.EnableV6 {
		if err := bindN("udp6", host6, &c6); err != nil {
			if strict {
				rollback()
				return 0, nil, nil, err
			}
			for _, c := range c6 {
				c.Close()
			}
			c6 = nil
			lastErr = err
		}
	}
	if len(c4) == 0 && len(c6) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no address family bound")
		}
		return 0, nil, nil, lastErr
	}
	return port, c4, c6, nil
}

func (t *Transport) startWorkers() {
	spawn := func(conns []*net.UDPConn, fam Family) {
		if len(conns) == 0 {
			return
		}
		if reusePort {
			// One worker per socket: kernel load-balances, no shared-socket contention.
			for _, c := range conns {
				t.wg.Add(1)
				go t.readLoop(c, fam)
			}
		} else {
			// Single socket, multiple readers (UDPConn is safe for concurrent use).
			for i := 0; i < t.workers; i++ {
				t.wg.Add(1)
				go t.readLoop(conns[0], fam)
			}
		}
	}
	spawn(t.conns4, V4)
	spawn(t.conns6, V6)
	// One reader per extra-port socket, in both reuseport and single-socket modes.
	for _, ec := range t.extra {
		t.wg.Add(1)
		go t.readLoopExtra(ec.c, ec.fam)
	}
}

// readLoopExtra serves an additional listen port: same as readLoop, but it
// remembers which socket each remote arrived on so replies egress from it.
func (t *Transport) readLoopExtra(c *net.UDPConn, fam Family) {
	defer t.wg.Done()
	for {
		bufp := t.pool.Get().(*[]byte)
		buf := *bufp
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.pool.Put(bufp)
			if t.closed.Load() {
				return
			}
			t.log.Debugf("transport: read error: %v", err)
			continue
		}
		t.rxPackets.Add(1)
		ap := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		t.recordSendFrom(ap, c)
		t.handler(buf[:n], ap, fam)
		t.pool.Put(bufp)
	}
}

// recordSendFrom remembers the socket a remote reached us on, so Send routes
// replies back out the same local port. Capped to bound memory.
func (t *Transport) recordSendFrom(from netip.AddrPort, c *net.UDPConn) {
	t.sendMu.Lock()
	if _, ok := t.sendFrom[from]; !ok && len(t.sendFrom) >= maxSendFrom {
		t.sendMu.Unlock()
		return
	}
	t.sendFrom[from] = c
	t.sendMu.Unlock()
}

func (t *Transport) readLoop(c *net.UDPConn, fam Family) {
	defer t.wg.Done()
	for {
		bufp := t.pool.Get().(*[]byte)
		buf := *bufp
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.pool.Put(bufp)
			if t.closed.Load() {
				return
			}
			// transient read error; keep serving
			t.log.Debugf("transport: read error: %v", err)
			continue
		}
		t.rxPackets.Add(1)
		t.handler(buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), fam)
		t.pool.Put(bufp)
	}
}

// Send transmits a datagram to a destination, choosing the socket whose family
// matches the destination address.
func (t *Transport) Send(to netip.AddrPort, payload []byte) error {
	if t.closed.Load() {
		return fmt.Errorf("transport: closed")
	}
	// Normalize 4-in-6 (::ffff:a.b.c.d) to plain IPv4 so it routes to and is
	// accepted by the v4 socket.
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	var conn *net.UDPConn
	if to.Addr().Is4() {
		if len(t.conns4) == 0 {
			return fmt.Errorf("transport: no IPv4 socket for %s", to)
		}
		conn = t.conns4[t.txRR4.Add(1)%uint64(len(t.conns4))]
	} else {
		if len(t.conns6) == 0 {
			return fmt.Errorf("transport: no IPv6 socket for %s", to)
		}
		conn = t.conns6[t.txRR6.Add(1)%uint64(len(t.conns6))]
	}
	// If this remote first reached us on an extra listen port, answer from that
	// same socket so its stateful firewall/NAT accepts the reply. The key is the
	// exact addr:port, so the recorded socket's family always matches `to`.
	if t.hasExtra {
		t.sendMu.RLock()
		if c := t.sendFrom[to]; c != nil {
			conn = c
		}
		t.sendMu.RUnlock()
	}
	if _, err := conn.WriteToUDPAddrPort(payload, to); err != nil {
		return err
	}
	t.txPackets.Add(1)
	return nil
}

// Port returns the bound UDP port.
func (t *Transport) Port() int { return t.port }

// Stats returns cumulative rx/tx datagram counts.
func (t *Transport) Stats() (rx, tx uint64) {
	return t.rxPackets.Load(), t.txPackets.Load()
}

// Close stops the workers and closes all sockets.
func (t *Transport) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	for _, c := range t.conns4 {
		c.Close()
	}
	for _, c := range t.conns6 {
		c.Close()
	}
	for _, ec := range t.extra {
		ec.c.Close()
	}
	t.wg.Wait()
	return nil
}
