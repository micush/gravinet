package mesh

import (
	"sync"
	"testing"
	"time"
)

// makeL4 builds a minimal IPv4 packet carrying a TCP/UDP header with the given
// ports and DSCP, for classifier tests.
func makeL4(proto uint8, srcPort, dstPort uint16, dscp int) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	p[1] = byte(dscp << 2)
	total := uint16(len(p))
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64
	p[9] = proto
	copy(p[12:16], []byte{10, 0, 0, 1})
	copy(p[16:20], []byte{10, 0, 0, 2})
	p[20], p[21] = byte(srcPort>>8), byte(srcPort)
	p[22], p[23] = byte(dstPort>>8), byte(dstPort)
	return p
}

func TestClassifier(t *testing.T) {
	c := NewClassifier(3, 2, []ClassRule{
		{Proto: 6, PortMin: 22, PortMax: 22, DSCP: -1, Class: 0}, // SSH → highest
		{Proto: 0, DSCP: 46, Class: 0},                           // EF-marked → highest
		{Proto: 17, DSCP: -1, Class: 1},                          // other UDP → middle
	}, nil)
	cases := []struct {
		name string
		pkt  []byte
		want int
	}{
		{"ssh", makeL4(6, 5000, 22, 0), 0},
		{"https-default", makeL4(6, 5000, 443, 0), 2},
		{"dns-udp", makeL4(17, 5000, 53, 0), 1},
		{"ef-marked-tcp", makeL4(6, 5000, 443, 46), 0},
		{"icmp-default", func() []byte { p := makeL4(1, 0, 0, 0); return p }(), 2},
	}
	for _, c2 := range cases {
		if got := c.classify(c2.pkt); got != c2.want {
			t.Errorf("%s: classify = %d, want %d", c2.name, got, c2.want)
		}
	}

	// A nil classifier always returns class 0 and reports a single class.
	var nilC *classifier
	if nilC.classify(makeL4(6, 1, 2, 0)) != 0 || nilC.numClasses() != 1 {
		t.Fatal("nil classifier should be single-class")
	}
}

// TestQoSPriorityScheduling enqueues low- and high-priority packets, then starts
// the drainer; strict priority must drain all high-priority traffic first.
func TestQoSPriorityScheduling(t *testing.T) {
	c := NewClassifier(2, 1, []ClassRule{
		{Proto: 6, PortMin: 22, PortMax: 22, DSCP: -1, Class: 0}, // SSH = high
	}, nil)
	var (
		mu    sync.Mutex
		order []uint16
	)
	s := newShaper(10_000_000, 0, 0, c, func(_ *peerSession, pkt []byte) {
		_, _, dp, _, _ := parseL4(pkt)
		mu.Lock()
		order = append(order, dp)
		mu.Unlock()
	})

	// Enqueue interleaved low/high BEFORE the drainer starts.
	for i := 0; i < 3; i++ {
		if !s.enqueue(nil, makeL4(17, 4000, 9999, 0)) { // bulk UDP = low
			t.Fatal("enqueue low dropped")
		}
		if !s.enqueue(nil, makeL4(6, 4000, 22, 0)) { // SSH = high
			t.Fatal("enqueue high dropped")
		}
	}

	go s.run()
	defer s.close()

	ok := waitUntil(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 6
	})
	if !ok {
		t.Fatalf("drainer did not emit all packets: %v", order)
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 3; i++ {
		if order[i] != 22 {
			t.Fatalf("position %d should be high-priority (port 22), got order %v", i, order)
		}
	}
	for i := 3; i < 6; i++ {
		if order[i] != 9999 {
			t.Fatalf("position %d should be low-priority (port 9999), got order %v", i, order)
		}
	}
}

// makeL4v6 builds a minimal IPv6 packet with the given next-header/ports/DSCP.
func makeL4v6(proto uint8, srcPort, dstPort uint16, dscp int) []byte {
	p := make([]byte, 44)
	tc := byte(dscp << 2)
	p[0] = 0x60 | (tc >> 4)
	p[1] = (tc & 0x0f) << 4
	p[6] = proto // next header
	p[7] = 64    // hop limit
	p[40], p[41] = byte(srcPort>>8), byte(srcPort)
	p[42], p[43] = byte(dstPort>>8), byte(dstPort)
	return p
}

// TestClassifierIPv6 confirms the same proto/port/DSCP rules classify IPv6
// traffic (QoS is family-agnostic; the shaper enqueues v4 and v6 alike).
func TestClassifierIPv6(t *testing.T) {
	c := NewClassifier(3, 2, []ClassRule{
		{Proto: 6, PortMin: 22, PortMax: 22, DSCP: -1, Class: 0},
		{Proto: 0, DSCP: 46, Class: 0},
		{Proto: 17, DSCP: -1, Class: 1},
	}, nil)
	cases := []struct {
		name string
		pkt  []byte
		want int
	}{
		{"v6-ssh", makeL4v6(6, 5000, 22, 0), 0},
		{"v6-https-default", makeL4v6(6, 5000, 443, 0), 2},
		{"v6-dns-udp", makeL4v6(17, 5000, 53, 0), 1},
		{"v6-ef-marked", makeL4v6(6, 5000, 443, 46), 0},
	}
	for _, c2 := range cases {
		if got := c.classify(c2.pkt); got != c2.want {
			t.Errorf("%s: classify = %d, want %d", c2.name, got, c2.want)
		}
	}
}

// TestDefaultClassDSCP checks the standard-codepoint ladder for the built-in
// 5-class default (classes=5, defClass=3): highest gets EF, the two classes
// between highest and default step down through the AFx1 tier, defClass
// itself gets CS0, and lowest gets CS1.
func TestDefaultClassDSCP(t *testing.T) {
	want := map[int]int{0: 46, 1: 34, 2: 26, 3: 0, 4: 8}
	for class, dscp := range want {
		if got := DefaultClassDSCP(class, 5, 3); got != dscp {
			t.Errorf("class %d: DefaultClassDSCP = %d, want %d", class, got, dscp)
		}
	}
}

// TestDefaultClassDSCPDegenerate checks classes counts too small to give
// defClass a distinct "middle" position don't panic or collapse onto
// duplicate/out-of-range codepoints.
func TestDefaultClassDSCPDegenerate(t *testing.T) {
	cases := []struct {
		classes, defClass int
	}{
		{1, 0}, {2, 0}, {2, 1}, {3, 2}, {3, 0},
	}
	for _, c := range cases {
		for class := 0; class < c.classes; class++ {
			dscp := DefaultClassDSCP(class, c.classes, c.defClass)
			if dscp < 0 || dscp > 63 {
				t.Errorf("classes=%d defClass=%d class=%d: DefaultClassDSCP = %d, out of 0-63 range",
					c.classes, c.defClass, class, dscp)
			}
		}
	}
}

// TestSetDSCPIPv4 checks setDSCP writes the DSCP bits while preserving ECN,
// and leaves the header checksum valid.
func TestSetDSCPIPv4(t *testing.T) {
	pkt := makeL4(6, 1234, 443, 0)
	pkt[1] |= 0x02 // set ECN bit, should survive the mark

	setDSCP(pkt, 46) // EF
	if got := pkt[1] >> 2; got != 46 {
		t.Fatalf("DSCP = %d, want 46", got)
	}
	if got := pkt[1] & 0x03; got != 0x02 {
		t.Fatalf("ECN bits = %#x, want preserved 0x02", got)
	}
	ihl := int(pkt[0]&0x0f) * 4
	if c := ones(pkt[:ihl], 0); c != 0 {
		t.Fatalf("IPv4 header checksum invalid after mark: ones() = %#x, want 0", c)
	}

	// Re-parsing with parseL4 should see the new value.
	_, _, _, dscp, ok := parseL4(pkt)
	if !ok || dscp != 46 {
		t.Fatalf("parseL4 after mark: dscp=%d ok=%v, want 46/true", dscp, ok)
	}
}

// TestSetDSCPIPv6 checks the IPv6 traffic-class bit layout (split across the
// low nibble of byte 0 and the high nibble of byte 1) and ECN preservation.
func TestSetDSCPIPv6(t *testing.T) {
	pkt := makeL4v6(6, 1234, 443, 0)
	pkt[1] |= 0x20 // ECN=0b10 lives in bits 5:4 of byte 1 (TC's low nibble, top-justified)

	setDSCP(pkt, 46) // EF
	_, _, _, dscp, ok := parseL4(pkt)
	if !ok || dscp != 46 {
		t.Fatalf("parseL4 after mark: dscp=%d ok=%v, want 46/true", dscp, ok)
	}
	tc := (pkt[0]&0x0f)<<4 | (pkt[1] >> 4)
	if ecn := tc & 0x03; ecn != 0x02 {
		t.Fatalf("ECN bits = %#x, want preserved 0x02", ecn)
	}
}

// TestSetDSCPNoopSkipsChecksumRewrite confirms re-marking a packet that
// already carries the target DSCP value is a true no-op: the checksum bytes
// (and the rest of the header) are left byte-for-byte untouched.
func TestSetDSCPNoopSkipsChecksumRewrite(t *testing.T) {
	pkt := makeL4(6, 1234, 443, 46)
	before := append([]byte(nil), pkt...)
	setDSCP(pkt, 46)
	for i := range pkt {
		if pkt[i] != before[i] {
			t.Fatalf("byte %d changed on a no-op mark: %#x -> %#x", i, before[i], pkt[i])
		}
	}
}

// TestShaperMarksDSCP confirms enqueue actually rewrites the drained packet's
// DSCP field to match its class, not just classifies it.
func TestShaperMarksDSCP(t *testing.T) {
	c := NewClassifier(5, 3, []ClassRule{
		{Proto: 6, PortMin: 22, PortMax: 22, DSCP: -1, Class: 0}, // SSH -> highest -> EF
	}, nil)
	var (
		mu   sync.Mutex
		dscp = -1
	)
	s := newShaper(10_000_000, 0, 0, c, func(_ *peerSession, pkt []byte) {
		_, _, _, d, _ := parseL4(pkt)
		mu.Lock()
		dscp = d
		mu.Unlock()
	})
	if !s.enqueue(nil, makeL4(6, 4000, 22, 0)) {
		t.Fatal("enqueue dropped")
	}
	go s.run()
	defer s.close()

	ok := waitUntil(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dscp != -1
	})
	if !ok {
		t.Fatal("drainer did not emit the packet")
	}
	mu.Lock()
	defer mu.Unlock()
	if dscp != 46 {
		t.Fatalf("drained packet DSCP = %d, want 46 (EF, class 0's default mark)", dscp)
	}
}

// TestClassifierDSCPOverride confirms a configured ClassDSCP override wins
// over the standard-codepoint default.
func TestClassifierDSCPOverride(t *testing.T) {
	c := NewClassifier(5, 3, nil, []int{-1, -1, 12, -1, -1}) // only class 2 overridden
	if got := c.dscpFor(0); got != 46 {
		t.Errorf("class 0 (no override) = %d, want default 46", got)
	}
	if got := c.dscpFor(2); got != 12 {
		t.Errorf("class 2 (overridden) = %d, want 12", got)
	}
	if got := c.dscpFor(3); got != 0 {
		t.Errorf("class 3 (no override) = %d, want default 0", got)
	}
}
