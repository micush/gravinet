package mesh

import (
	"bytes"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// fakeDev is an in-memory overlay interface for tests. Packets pushed to `in`
// are returned by Read (TUN→engine); packets the engine Writes land on `out`.
type fakeDev struct {
	name        string
	in          chan []byte
	out         chan []byte
	closed      chan struct{}
	once        sync.Once
	mu          sync.Mutex
	assigned4   netip.Addr
	assigned6   netip.Addr
	routes      map[netip.Prefix]bool
	routeMetric map[netip.Prefix]int
	mtu         int
}

func newFakeDev(name string) *fakeDev {
	return &fakeDev{name: name, in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{})}
}

func (d *fakeDev) Read(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, io.EOF
	case b := <-d.in:
		return copy(p, b), nil
	}
}
func (d *fakeDev) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	select {
	case <-d.closed:
		return 0, io.EOF
	case d.out <- cp:
	default:
	}
	return len(p), nil
}
func (d *fakeDev) Name() string { return d.name }
func (d *fakeDev) MTU() int {
	if d.mtu > 0 {
		return d.mtu
	}
	return 1400
}
func (d *fakeDev) Close() error { d.once.Do(func() { close(d.closed) }); return nil }
func (d *fakeDev) AddIPv4(a netip.Addr, prefix int) error {
	d.mu.Lock()
	d.assigned4 = a
	d.mu.Unlock()
	return nil
}
func (d *fakeDev) AddIPv6(a netip.Addr, prefix int) error {
	d.mu.Lock()
	d.assigned6 = a
	d.mu.Unlock()
	return nil
}
func (d *fakeDev) AddRoute(p netip.Prefix, metric int) error {
	d.mu.Lock()
	if d.routes == nil {
		d.routes = map[netip.Prefix]bool{}
	}
	if d.routeMetric == nil {
		d.routeMetric = map[netip.Prefix]int{}
	}
	d.routes[p] = true
	d.routeMetric[p] = metric
	d.mu.Unlock()
	return nil
}
func (d *fakeDev) DelRoute(p netip.Prefix, metric int) error {
	d.mu.Lock()
	delete(d.routes, p)
	delete(d.routeMetric, p)
	d.mu.Unlock()
	return nil
}

// IfIndex returns a fixed, obviously-fake interface index — tests that need
// to exercise full-tunnel bypass-route logic can rely on this exact value
// rather than a real OS ifindex.
func (d *fakeDev) IfIndex() (int32, error) { return 0xF4CE, nil }
func (d *fakeDev) metricOf(p netip.Prefix) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.routeMetric[p]
}
func (d *fakeDev) hasRoute(p netip.Prefix) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.routes[p]
}
func (d *fakeDev) addr4() netip.Addr {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.assigned4
}

// makeIPv4 builds a minimal valid IPv4 packet with the given addresses.
func makeIPv4(src, dst netip.Addr, payload []byte) []byte {
	p := make([]byte, 20+len(payload))
	p[0] = 0x45 // version 4, IHL 5
	total := uint16(len(p))
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64 // TTL
	p[9] = 17 // UDP
	sa := src.As4()
	da := dst.As4()
	copy(p[12:16], sa[:])
	copy(p[16:20], da[:])
	copy(p[20:], payload)
	return p
}

func TestPointToPointTunnel(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xABCD1234)
	addrA := netip.MustParseAddr("10.77.0.1")
	addrB := netip.MustParseAddr("10.77.0.2")

	devA := newFakeDev("a")
	devB := newFakeDev("b")

	mkEngine := func(node string, dev *fakeDev, self netip.Addr) (*Engine, *transport.Transport) {
		ks, _ := crypto.NewKeySet([]string{key})
		eng := NewEngine(Options{
			NodeID:   node,
			Hostname: node + ".test",
			Nets:     []NetSpec{{ID: netID, Name: "t", Keys: ks, Dev: dev, Self4: self}},
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
			Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatalf("transport open: %v", err)
		}
		eng.Attach(tr)
		eng.Start()
		return eng, tr
	}

	engA, trA := mkEngine("A", devA, addrA)
	engB, trB := mkEngine("B", devB, addrB)
	defer func() {
		devA.Close()
		devB.Close()
		engA.Stop()
		engB.Stop()
		trA.Close()
		trB.Close()
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	engA.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trB.Port())))
	engB.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trA.Port())))

	// Wait for both sides to establish a session.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if engA.SessionCount() > 0 && engB.SessionCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if engA.SessionCount() == 0 || engB.SessionCount() == 0 {
		t.Fatalf("handshake did not complete: A=%d B=%d", engA.SessionCount(), engB.SessionCount())
	}

	// Push an overlay packet A→B and confirm it arrives intact on B's interface.
	payload := []byte("encrypted-overlay-payload-12345")
	pkt := makeIPv4(addrA, addrB, payload)
	devA.in <- pkt

	select {
	case got := <-devB.out:
		if !bytes.Equal(got, pkt) {
			t.Fatalf("delivered packet differs:\n got=%x\nwant=%x", got, pkt)
		}
		if !bytes.Contains(got, payload) {
			t.Fatal("payload missing from delivered packet")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("packet did not traverse the tunnel")
	}

	// Confirm it actually went over UDP (transport counters moved).
	rxA, txA := trA.Stats()
	if txA == 0 || rxA == 0 {
		t.Fatalf("transport A shows no traffic: rx=%d tx=%d", rxA, txA)
	}
}
