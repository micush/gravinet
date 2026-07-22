package mesh

import (
	"os"
	"runtime"
)

// gsoDevice is an optional capability of the attached Device — implemented
// by *tun.Device on 64-bit Linux (see internal/tun/vnethdr_linux.go,
// gsosplit.go, grocoalesce.go), absent on every other platform and on every
// test fake. Same pattern as fallbackDialer above: the engine type-asserts
// for it, and a Device without it just never gets offered GSO reads/writes —
// tunLoop keeps calling Read, deliverInner keeps calling Write, exactly as
// before this file existed.
//
// This is deliberately a narrow, mesh-package-local interface rather than an
// import of internal/tun's own types, so the "engine stays decoupled from
// internal/tun" property NewDevice's doc comment describes stays true — see
// CoalesceWrite/FlushCoalesced's doc comment in grocoalesce.go for the other
// half of that seam.
type gsoDevice interface {
	GSOEnabled() bool
	EnableGSO() error
	ReadPackets(buf []byte, emit func([]byte)) error
	CoalesceWrite(pkt []byte) error
	FlushCoalesced() error
}

// tunGSORequested reports whether the TUN GSO/GRO fast path (gravinet's
// Phase C — see docs/changelog.md) should be attempted. Defaults on as of
// v573: v572 shipped this opt-in, on the reasoning that Phase A only ever
// changed how many syscalls carried the same bytes while this changes the
// bytes (splitting/merging TCP segments with hand-rolled checksum
// arithmetic — see internal/tun/gsomath_linux.go), and that risk hadn't had
// a real bulk-transfer field test behind it yet. v573 defaults it on at the
// operator's explicit request, made with that gap already known — not
// because the gap closed. Set GRAVINET_TUN_GSO=0 to fall back to the
// original per-packet path if a field issue turns up; that is the
// documented way back to v572's behavior, not a hidden or partial one.
func tunGSORequested() bool { return os.Getenv("GRAVINET_TUN_GSO") != "0" }

// tunGSOWorthy reports whether this process has enough cores for the
// coalesce-then-write pipeline to pay for itself — same reasoning as
// batch_linux.go's initBatch gating UDP batching on GOMAXPROCS>=2: on one
// core, the flusher goroutine has nothing to run concurrently with, so
// draining the ring is pure added latency and context-switch cost with no
// real batching ever forming.
func tunGSOWorthy() bool { return runtime.GOMAXPROCS(0) >= 2 }

// maybeEnableGSO negotiates GSO on d if GRAVINET_TUN_GSO isn't set to "0",
// this process has cores to spare, and d actually supports it (every
// non-Linux platform and every test fake do not). Called from setDev, so it runs once per
// device install — including every data-plane rebuild, since a torn-down
// and recreated tun gets a fresh fd with offload not yet negotiated on it.
// Logs once per attempt; never fatal — a negotiation failure just leaves
// this device on the unmodified per-packet path, identical to before this
// feature existed.
func (e *Engine) maybeEnableGSO(ns *netState, d Device) {
	gd, ok := d.(gsoDevice)
	if !ok || !tunGSORequested() || !tunGSOWorthy() {
		return
	}
	if err := gd.EnableGSO(); err != nil {
		e.log.Infof("mesh: tun gso=off (net %016x, iface %s): %v", ns.spec.ID, d.Name(), err)
		return
	}
	e.log.Infof("mesh: tun gso=on (net %016x, iface %s)", ns.spec.ID, d.Name())
}

// tunGSOActive reports whether this network's current device has GSO
// negotiated right now — the per-call check deliverInner and tunLoop use to
// decide whether to take the fast path at all. Cheap: one interface type
// assertion and one bool method call, safe to do on every packet.
func (ns *netState) tunGSOActive() (gsoDevice, bool) {
	gd, ok := ns.dev().(gsoDevice)
	if !ok || !gd.GSOEnabled() {
		return nil, false
	}
	return gd, true
}

// readTunPackets reads one frame from dev and calls emit once per resulting
// packet: once, with a plain Read's result, for any Device that doesn't
// support GSO splitting or doesn't currently have it negotiated (which is
// every platform besides 64-bit Linux, every test fake, and the common case
// of GRAVINET_TUN_GSO=0 having been set) — identical to what every
// tunLoop variant did before this helper existed, no extra copy, no extra
// branch cost beyond one cheap type assertion. Only when gd.GSOEnabled() is
// actually true does this call ReadPackets, which may emit multiple
// segments from one read (see gsosplit.go).
func readTunPackets(dev Device, buf []byte, emit func([]byte)) error {
	if gd, ok := dev.(gsoDevice); ok && gd.GSOEnabled() {
		return gd.ReadPackets(buf, emit)
	}
	n, err := dev.Read(buf)
	if err != nil {
		return err
	}
	emit(buf[:n])
	return nil
}

// ---- write-side flusher ----

const tunRingSize = 256 // power of two; same order as transport's txRingSize

// tunFlusher owns one network's TUN write ring and turns queued decrypted
// packets into coalesced (or, when they don't coalesce, plain) writes.
// Exactly one runs per network, started only when maybeEnableGSO actually
// turned GSO on for that network's device — see startNetwork/setDev.
type tunFlusher struct {
	e    *Engine
	ns   *netState
	ring *tunRing
}

// run drains the ring on every wakeup until told to stop, then makes one
// final best-effort pass so packets enqueued during shutdown still reach
// the device — same shape as transport's flusher.run. Two distinct signals
// mean "stop": ns.done (this one network being removed live) and e.stop
// (the whole engine stopping) — tunLoop's own loop selects on both for the
// same reason (see its top-of-loop select), and missing either one here
// would leave this goroutine blocked forever on the other, which is exactly
// what happened during development: Engine.Stop only closes e.stop, never
// ns.done, so a version of this that only selected on ns.done leaked the
// goroutine and hung ns.wg.Wait() on every plain Stop().
func (f *tunFlusher) run() {
	defer f.ns.wg.Done()
	for {
		select {
		case <-f.ring.sig:
			f.drain()
		case <-f.ns.done:
			f.drain()
			return
		case <-f.e.stop:
			f.drain()
			return
		}
	}
}

func (f *tunFlusher) drain() {
	gd, ok := f.ns.tunGSOActive()
	if !ok {
		// GSO was turned off under us (device rebuilt onto a fresh fd that
		// hasn't been re-negotiated yet, or the platform simply doesn't
		// support it). Fall back to writing whatever's queued directly
		// through the plain Device.Write path rather than stalling it in
		// the ring until GSO comes back — deliverInner only enqueues here
		// in the first place when tunGSOActive was true at enqueue time,
		// but that can go stale between enqueue and drain.
		f.drainDirect()
		return
	}
	const maxBatch = 256
	for {
		start, n := f.ring.claim(maxBatch)
		if n == 0 {
			return
		}
		for i := uint64(0); i < n; i++ {
			s := &f.ring.slots[(start+i)&f.ring.mask]
			if err := gd.CoalesceWrite(s.buf[:s.n]); err != nil {
				f.e.log.Debugf("mesh: tun coalesced write (net %016x): %v", f.ns.spec.ID, err)
			}
		}
		if err := gd.FlushCoalesced(); err != nil {
			f.e.log.Debugf("mesh: tun coalesced flush (net %016x): %v", f.ns.spec.ID, err)
		}
		f.ring.release(n)
	}
}

// drainDirect is drain's fallback when the device no longer reports GSO
// active — write every queued packet through the ordinary per-packet path.
func (f *tunFlusher) drainDirect() {
	const maxBatch = 256
	for {
		start, n := f.ring.claim(maxBatch)
		if n == 0 {
			return
		}
		dev := f.ns.dev()
		for i := uint64(0); i < n; i++ {
			s := &f.ring.slots[(start+i)&f.ring.mask]
			if _, err := dev.Write(s.buf[:s.n]); err != nil {
				f.e.log.Debugf("mesh: tun write (net %016x): %v", f.ns.spec.ID, err)
			}
		}
		f.ring.release(n)
	}
}
