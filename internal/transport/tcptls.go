package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gravinet/internal/logx"
)

// tlsReadIdle bounds how long a TLS fallback connection may sit with no frame
// arriving before its read loop gives up and tears the connection down. This
// is the fix for a terminal leak: readFrame (frame.go) blocks in io.ReadFull
// with no deadline, so a fallback connection that goes silent — a TCP session
// that completed to a peer's now-stale post-roam endpoint but over which no
// mesh frame will ever arrive again — parks its read goroutine *forever*, and
// because the connection stays registered in t.conns the engine believes it
// still has a working fallback to that peer and never redials. Across repeated
// roams these accumulate (observed: 30+ leaked outbound read goroutines, all
// blocked 5+ minutes, one per stale dial), and the mesh stays partitioned
// with every peer masked as "reachable via a dead pipe" until a restart clears
// them — the reported terminal state that no further roaming recovers.
//
// The window derives from the mesh's own liveness contract: a 20s keepalive
// with a 75s peerTimeout (see disableNagle's comment and internal/mesh). A
// connection carrying a live session sees a keepalive frame every 20s and so
// resets this deadline long before it expires; one that hasn't produced a
// single frame in 90s is, by the mesh's own definition, dead — so dropping it
// is correct and frees the peer to be redialed. A rolling deadline (reset
// before every readFrame) means an active connection is never affected; only a
// genuinely idle one hits it. A var, not a const, only so tests can shorten
// it (see TestTLSIdleConnectionTimesOut); never reassigned in production.
var tlsReadIdle = 90 * time.Second

// readFrameIdle is readFrame with a rolling idle deadline applied to c first,
// so a silent connection returns an error (i/o timeout) instead of blocking
// forever. Any error — timeout, EOF, or a real read error — ends the caller's
// read loop, which unregisters and closes the connection.
func readFrameIdle(c net.Conn) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(tlsReadIdle))
	return readFrame(c)
}

// disableNagle turns off Nagle's algorithm on a raw TCP connection before TLS
// wraps it. Without this, Go's default net.TCPConn behaves like most stacks'
// default socket and leaves Nagle on, which delays small, infrequent writes
// waiting to either coalesce with more outgoing data or piggyback on a
// pending ACK. A steady mesh keepalive every 20s (see keepaliveInterval in
// internal/mesh) never notices — nothing about a 75s peerTimeout cares about
// an extra 40-200ms. But every frame on this fallback carries exactly that
// profile: small, sporadic mesh datagrams (a handshake message, a keepalive,
// a redistributed route, an overlay ping) rather than a continuous bulk
// stream Nagle is meant to help. Combined with delayed ACKs on the other
// end, that per-write latency can stack in both directions of a real
// round-trip. For most of what rides this transport that's invisible; for
// anything with a tight timeout budget bracketing this exact stream — e.g.
// the web admin's overlay ping (2 probes, ~1s each) to a peer already
// reached over this fallback because its direct UDP path is blocked, which
// is disproportionately likely to also mean a longer, real-world-noisier
// path to begin with — it's the difference between a reply arriving in time
// and a reported timeout despite the mesh session being perfectly healthy.
// Best-effort: not every net.Conn is a *net.TCPConn (a future dialer might
// wrap something else), so a failed assertion just leaves Nagle's default
// behavior in place rather than erroring out here.
func disableNagle(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// TLSTransport is gravinet's TCP/TLS fallback underlay. Every node listens here
// (default :443) in addition to the UDP transport, so a peer on a network that
// blocks UDP can still reach the mesh over what looks like an ordinary HTTPS
// connection. It carries the exact same datagrams as UDP, framed on the stream
// (see frame.go), and delivers them to the same Handler, so the engine processes
// TCP-borne packets identically to UDP ones (same handshake auth, same rate
// limiting — no bypass).
//
// TLS here is camouflage and stream integrity, not the trust boundary: the mesh
// PSK and per-session handshake remain the real authentication, so dials use a
// self-signed cert and skip verification.
type TLSTransport struct {
	port    int
	handler Handler
	log     *logx.Logger
	tlsCfg  *tls.Config

	ln       net.Listener   // primary
	extraLns []net.Listener // config extra_tcp_listen_ports — see TLSOptions.ExtraPorts

	mu    sync.RWMutex
	conns map[netip.AddrPort]*tlsConn // remote endpoint -> live connection

	dialMu  sync.Mutex // serializes dial attempts per remote
	dialing map[netip.AddrPort]bool

	closed atomic.Bool
	wg     sync.WaitGroup

	rxPackets atomic.Uint64
	txPackets atomic.Uint64
}

// tlsConn pairs a framed writer with the underlying connection for teardown.
type tlsConn struct {
	fc  *frameConn
	raw net.Conn
}

// TLSOptions configure a TLSTransport.
type TLSOptions struct {
	BindAddr string // host to bind; "" => all interfaces
	Port     int    // listen port (default 443)
	// ExtraPorts are additional TCP/TLS listeners bound *in addition* to Port,
	// the TCP-side equivalent of transport.Options.ExtraPorts on the UDP
	// transport — same motivation (a peer behind a restrictive firewall can
	// reach this node on a well-known port like 80 even if it's already using
	// 443 for the primary fallback), same best-effort semantics: a port that
	// can't bind (privileged or in use) is logged and skipped, never fatal —
	// see OpenTLS, which now treats Port itself the identical way.
	// Unlike the UDP case, no reply-routing bookkeeping is needed here — every
	// accepted TCP connection is already its own net.Conn, registered by
	// remote address regardless of which listener accepted it, so Send
	// already writes back down the connection a peer actually dialed.
	ExtraPorts []int
	Handler    Handler
	Log        *logx.Logger
}

// OpenTLS binds the TCP/TLS listener(s) and starts accepting. Every configured
// port — Port and each of ExtraPorts — is best-effort: one already held by
// something else (another process, a stale gravinet instance, a privileged-
// port restriction) is logged and skipped, and binding moves on to the next
// configured port rather than aborting. That includes Port itself, so a busy
// primary fallback port no longer takes every configured extra TCP port down
// with it. OpenTLS only fails outright if nothing at all — neither Port nor
// any ExtraPorts entry — could bind; the caller (main.go) already treats that
// as non-fatal and continues UDP-only.
func OpenTLS(o TLSOptions) (*TLSTransport, error) {
	if o.Handler == nil {
		return nil, fmt.Errorf("transport: TLS handler is nil")
	}
	cert, err := selfSignedCert()
	if err != nil {
		return nil, fmt.Errorf("transport: generate TLS cert: %w", err)
	}
	t := &TLSTransport{
		port:    o.Port,
		handler: o.Handler,
		log:     o.Log,
		tlsCfg:  &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		conns:   make(map[netip.AddrPort]*tlsConn),
		dialing: make(map[netip.AddrPort]bool),
	}

	seenPort := map[int]bool{}
	ln, err := net.Listen("tcp", net.JoinHostPort(o.BindAddr, fmt.Sprintf("%d", o.Port)))
	if err != nil {
		if t.log != nil {
			t.log.Warnf("transport: tls fallback port %d not bound (%v) — skipping, trying any configured extra ports", o.Port, err)
		}
	} else {
		t.ln = ln
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			t.port = ta.Port // resolve the actual port (o.Port==0 binds an ephemeral one)
		}
		seenPort[t.port] = true
		t.wg.Add(1)
		go t.acceptLoop(t.ln)
		if t.log != nil {
			t.log.Infof("transport: also listening on tcp port %d (TLS fallback)", t.port)
		}
	}

	for _, ep := range o.ExtraPorts {
		if ep <= 0 || ep > 65535 || seenPort[ep] {
			continue
		}
		seenPort[ep] = true
		eln, err := net.Listen("tcp", net.JoinHostPort(o.BindAddr, fmt.Sprintf("%d", ep)))
		if err != nil {
			if t.log != nil {
				t.log.Warnf("transport: extra tls listen port %d not bound (%v) — skipping", ep, err)
			}
			continue
		}
		t.extraLns = append(t.extraLns, eln)
		t.wg.Add(1)
		go t.acceptLoop(eln)
		if t.log != nil {
			t.log.Infof("transport: also listening on tcp port %d (TLS fallback, extra)", ep)
		}
	}

	if t.ln == nil && len(t.extraLns) == 0 {
		return nil, fmt.Errorf("transport: no tls listener could be bound (tried primary tcp/%d and %d extra port(s))", o.Port, len(o.ExtraPorts))
	}
	return t, nil
}

func (t *TLSTransport) acceptLoop(ln net.Listener) {
	defer t.wg.Done()
	for {
		raw, err := ln.Accept()
		if err != nil {
			if t.closed.Load() {
				return
			}
			if t.log != nil {
				t.log.Debugf("transport: tls accept: %v", err)
			}
			continue
		}
		t.wg.Add(1)
		go t.serve(raw)
	}
}

// serve completes the TLS handshake on an accepted connection and reads frames
// from it until it closes.
func (t *TLSTransport) serve(raw net.Conn) {
	defer t.wg.Done()
	disableNagle(raw)
	srv := tls.Server(raw, t.tlsCfg)
	_ = srv.SetDeadline(time.Now().Add(10 * time.Second))
	if err := srv.Handshake(); err != nil {
		srv.Close()
		return
	}
	_ = srv.SetDeadline(time.Time{})
	t.readConn(srv)
}

// readConn registers a connection by its remote endpoint and pumps frames into
// the handler until the connection ends, then unregisters it.
func (t *TLSTransport) readConn(c net.Conn) {
	ap, ok := remoteAddrPort(c)
	if !ok {
		c.Close()
		return
	}
	fam := V4
	if ap.Addr().Is6() {
		fam = V6
	}
	t.register(ap, c)
	defer t.unregister(ap, c)
	for {
		if t.closed.Load() {
			return
		}
		payload, err := readFrameIdle(c)
		if err != nil {
			return
		}
		t.rxPackets.Add(1)
		t.handler(payload, ap, fam)
	}
}

func (t *TLSTransport) register(ap netip.AddrPort, c net.Conn) {
	tc := &tlsConn{fc: &frameConn{w: c}, raw: c}
	t.mu.Lock()
	if old := t.conns[ap]; old != nil && old.raw != c {
		old.raw.Close()
	}
	t.conns[ap] = tc
	t.mu.Unlock()
}

func (t *TLSTransport) unregister(ap netip.AddrPort, c net.Conn) {
	t.mu.Lock()
	if tc := t.conns[ap]; tc != nil && tc.raw == c {
		delete(t.conns, ap)
	}
	t.mu.Unlock()
	c.Close()
}

// HasConn reports whether a live TLS connection to the endpoint exists.
func (t *TLSTransport) HasConn(to netip.AddrPort) bool {
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	t.mu.RLock()
	_, ok := t.conns[to]
	t.mu.RUnlock()
	return ok
}

// Send writes a framed datagram over the existing TLS connection to `to`. It
// returns an error if there is no live connection (the caller falls back to
// UDP); it never dials, so the data path stays non-blocking.
func (t *TLSTransport) Send(to netip.AddrPort, payload []byte) error {
	if t.closed.Load() {
		return fmt.Errorf("transport: tls closed")
	}
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	t.mu.RLock()
	tc := t.conns[to]
	t.mu.RUnlock()
	if tc == nil {
		return fmt.Errorf("transport: no tls conn for %s", to)
	}
	if err := tc.fc.writeFrame(payload); err != nil {
		t.unregister(to, tc.raw)
		return err
	}
	t.txPackets.Add(1)
	return nil
}

// Dial establishes an outbound TLS connection to a peer's fallback port and
// starts reading from it, so subsequent Sends to that endpoint go over TCP. Used
// by the engine to fall back when UDP can't reach a peer. Idempotent and safe to
// call concurrently; a no-op if a connection already exists.
func (t *TLSTransport) Dial(to netip.AddrPort) error {
	if t.closed.Load() {
		return fmt.Errorf("transport: tls closed")
	}
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	if t.HasConn(to) {
		return nil
	}
	t.dialMu.Lock()
	if t.dialing[to] {
		t.dialMu.Unlock()
		return nil // a dial to this endpoint is already in flight
	}
	t.dialing[to] = true
	t.dialMu.Unlock()
	defer func() {
		t.dialMu.Lock()
		delete(t.dialing, to)
		t.dialMu.Unlock()
	}()

	d := &net.Dialer{Timeout: 8 * time.Second}
	raw, err := d.Dial("tcp", to.String())
	if err != nil {
		return err
	}
	disableNagle(raw)
	// ServerName is cosmetic (self-signed, unverified); set it so the handshake
	// looks like a normal SNI request rather than an empty one.
	c := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: "www.cloudflare.com", MinVersion: tls.VersionTLS12})
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		c.Close()
		return err
	}
	_ = c.SetDeadline(time.Time{})
	// Register before returning so the caller can immediately route to this conn.
	// Key by the dialed endpoint (the address the engine sends to), not our local
	// source port.
	t.register(to, c)
	fam := V4
	if to.Addr().Is6() {
		fam = V6
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer t.unregister(to, c)
		for {
			if t.closed.Load() {
				return
			}
			payload, err := readFrameIdle(c)
			if err != nil {
				return
			}
			t.rxPackets.Add(1)
			t.handler(payload, to, fam)
		}
	}()
	return nil
}

// Port returns the primary TCP fallback port. This is the configured/resolved
// port regardless of whether it actually ended up bound — see HasPrimary —
// since that's what the rest of gravinet advertises to peers either way
// (ExtraPorts entries are best-effort the same way and are advertised
// separately, independent of their own bind success).
func (t *TLSTransport) Port() int { return t.port }

// HasPrimary reports whether the primary port (as opposed to only some subset
// of ExtraPorts) actually got bound. False after a startup where something
// else already held it — the transport still runs on whichever extra ports
// did bind.
func (t *TLSTransport) HasPrimary() bool { return t.ln != nil }

// Stats reports framed datagrams received and sent over TLS.
func (t *TLSTransport) Stats() (rx, tx uint64) {
	return t.rxPackets.Load(), t.txPackets.Load()
}

// Close stops accepting, tears down all connections, and waits for goroutines.
func (t *TLSTransport) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	if t.ln != nil {
		t.ln.Close()
	}
	for _, ln := range t.extraLns {
		ln.Close()
	}
	t.mu.Lock()
	for _, tc := range t.conns {
		tc.raw.Close()
	}
	t.conns = make(map[netip.AddrPort]*tlsConn)
	t.mu.Unlock()
	t.wg.Wait()
	return nil
}

// remoteAddrPort extracts a netip.AddrPort from a connection's remote address.
func remoteAddrPort(c net.Conn) (netip.AddrPort, bool) {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ap := ta.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), ap.IsValid()
	}
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// selfSignedCert generates an in-memory ECDSA self-signed certificate for the
// TLS listener. No file/PEM round-trip is needed; the cert is ephemeral and
// regenerated each start (it is not an identity, just transport wrapping).
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "gravinet"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
