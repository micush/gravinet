package webadmin

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func ethIPv4TCP() []byte {
	pkt := make([]byte, 14+20+20)
	// Ethernet
	binary.BigEndian.PutUint16(pkt[12:], 0x0800)
	// IPv4
	ip := pkt[14:]
	ip[0] = 0x45 // version 4, IHL 5
	ip[9] = 6    // TCP
	copy(ip[12:16], []byte{10, 0, 0, 2})
	copy(ip[16:20], []byte{1, 1, 1, 1})
	// TCP
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], 51234) // sport
	binary.BigEndian.PutUint16(tcp[2:], 443)   // dport
	tcp[13] = 0x02                             // SYN
	return pkt
}

func TestSummarizeEthIPv4TCP(t *testing.T) {
	got := summarizePacket(linktypeEthernet, ethIPv4TCP())
	if !strings.Contains(got, "TCP 10.0.0.2.51234 > 1.1.1.1.443") || !strings.Contains(got, "[S]") {
		t.Fatalf("unexpected summary: %q", got)
	}
}

func TestSummarizeRawIPv4UDP(t *testing.T) {
	ip := make([]byte, 20+8)
	ip[0] = 0x45
	ip[9] = 17 // UDP
	copy(ip[12:16], []byte{192, 168, 1, 5})
	copy(ip[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(ip[20:], 5353)
	binary.BigEndian.PutUint16(ip[22:], 53)
	got := summarizePacket(linktypeRaw, ip)
	if !strings.Contains(got, "UDP 192.168.1.5.5353 > 8.8.8.8.53") {
		t.Fatalf("unexpected summary: %q", got)
	}
}

func TestSummarizeARP(t *testing.T) {
	pkt := make([]byte, 14)
	binary.BigEndian.PutUint16(pkt[12:], 0x0806)
	if got := summarizePacket(linktypeEthernet, pkt); !strings.HasPrefix(got, "ARP") {
		t.Fatalf("expected ARP, got %q", got)
	}
}

func TestPcapHeaderAndRecord(t *testing.T) {
	h := pcapGlobalHeader(capSnaplen, linktypeRaw)
	if binary.LittleEndian.Uint32(h[0:]) != 0xa1b2c3d4 {
		t.Fatal("bad pcap magic")
	}
	if binary.LittleEndian.Uint32(h[20:]) != linktypeRaw {
		t.Fatal("bad linktype in header")
	}
	if binary.LittleEndian.Uint32(h[16:]) != capSnaplen {
		t.Fatal("bad snaplen in header")
	}
	data := []byte{1, 2, 3, 4, 5}
	rec := pcapRecord(time.Unix(100, 123000), len(data), 9, data)
	if binary.LittleEndian.Uint32(rec[4:]) != 123 { // usec
		t.Fatalf("bad usec: %d", binary.LittleEndian.Uint32(rec[4:]))
	}
	if binary.LittleEndian.Uint32(rec[8:]) != uint32(len(data)) {
		t.Fatal("bad caplen")
	}
	if binary.LittleEndian.Uint32(rec[12:]) != 9 {
		t.Fatal("bad origlen")
	}
	if !bytes.Equal(rec[16:], data) {
		t.Fatal("payload mismatch")
	}
}

func TestWritePcapRoundTrip(t *testing.T) {
	cs := newCaptureState()
	cs.linktype = linktypeEthernet
	cs.addEpoch(0, time.Unix(1, 0), ethIPv4TCP())
	cs.addEpoch(0, time.Unix(2, 0), ethIPv4TCP())
	var buf bytes.Buffer
	cs.writePcap(&buf)
	b := buf.Bytes()
	if len(b) < 24 {
		t.Fatal("pcap too short")
	}
	if binary.LittleEndian.Uint32(b[0:]) != 0xa1b2c3d4 {
		t.Fatal("missing global header")
	}
	// Two records of 16 + 54 bytes each.
	want := 24 + 2*(16+54)
	if len(b) != want {
		t.Fatalf("pcap size = %d, want %d", len(b), want)
	}
}

func TestCaptureBufferCap(t *testing.T) {
	cs := newCaptureState()
	cs.linktype = linktypeRaw
	for i := 0; i < capMaxPackets+10; i++ {
		cs.addEpoch(0, time.Unix(int64(i), 0), []byte{0x45, 0, 0, 0, 0, 0, 0, 0, 0, 17, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 0, 1, 0, 2})
	}
	cs.mu.Lock()
	n := len(cs.buf)
	first := cs.buf[0].seq
	cs.mu.Unlock()
	if n != capMaxPackets {
		t.Fatalf("buffer length = %d, want %d", n, capMaxPackets)
	}
	if first != 11 { // first 10 dropped; seqs are 1-based
		t.Fatalf("oldest seq = %d, want 11", first)
	}
	pkts, cursor, _, _, _ := cs.since(int64(capMaxPackets), 3000)
	if cursor != int64(capMaxPackets+10) {
		t.Fatalf("cursor = %d", cursor)
	}
	if len(pkts) != 10 {
		t.Fatalf("since() returned %d, want 10", len(pkts))
	}
}

func TestCaptureEpochDropsStalePackets(t *testing.T) {
	cs := newCaptureState()
	cs.linktype = linktypeRaw
	cs.begin("mesh0", linktypeRaw)                    // epoch -> 1
	cs.addEpoch(0, time.Now(), []byte{0x45, 0, 0, 0}) // stale epoch, must be ignored
	cs.mu.Lock()
	n := len(cs.buf)
	cs.mu.Unlock()
	if n != 0 {
		t.Fatalf("stale-epoch packet was not dropped: len=%d", n)
	}
}

// nullFramedIPv4 builds linktypeNull's framing (4-byte AF_INET, host/little-
// endian order) around a minimal IPv4/UDP packet.
func nullFramedLE(af uint32, ipAndUp []byte) []byte {
	pkt := make([]byte, 4+len(ipAndUp))
	binary.LittleEndian.PutUint32(pkt[0:4], af)
	copy(pkt[4:], ipAndUp)
	return pkt
}

func loopFramedBE(af uint32, ipAndUp []byte) []byte {
	pkt := make([]byte, 4+len(ipAndUp))
	binary.BigEndian.PutUint32(pkt[0:4], af)
	copy(pkt[4:], ipAndUp)
	return pkt
}

func minimalIPv4UDP() []byte {
	ip := make([]byte, 20+8)
	ip[0] = 0x45
	ip[9] = 17 // UDP
	copy(ip[12:16], []byte{192, 168, 1, 5})
	copy(ip[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(ip[20:], 5353)
	binary.BigEndian.PutUint16(ip[22:], 53)
	return ip
}

func minimalIPv6UDP() []byte {
	ip := make([]byte, 40+8)
	ip[0] = 0x60 // version 6
	ip[6] = 17   // next header: UDP
	copy(ip[8:24], []byte{0xfd, 0, 0x02, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	copy(ip[24:40], []byte{0xfd, 0, 0x02, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})
	binary.BigEndian.PutUint16(ip[40:], 5353)
	binary.BigEndian.PutUint16(ip[42:], 53)
	return ip
}

// TestSummarizeNullAllBSDIPv6Values is the regression test for the "gn-
// freebsd/gn-macos captures show icmp6=0" finding: linktypeNull's AF_INET6
// value differs by BSD (24 OpenBSD, 28 FreeBSD, 30 macOS/Darwin — confirmed
// against each OS's own current sys/socket.h), and a saved file may be read
// on a different machine than captured it, so all three must decode as IPv6,
// not just whichever one the code happened to special-case first.
func TestSummarizeNullAllBSDIPv6Values(t *testing.T) {
	for _, af := range []uint32{24, 28, 30} {
		got := summarizePacket(linktypeNull, nullFramedLE(af, minimalIPv6UDP()))
		if !strings.Contains(got, "UDP") || !strings.Contains(got, "fd00:203::1.5353") {
			t.Errorf("AF_INET6=%d: unexpected summary: %q", af, got)
		}
	}
	got := summarizePacket(linktypeNull, nullFramedLE(2, minimalIPv4UDP()))
	if !strings.Contains(got, "UDP 192.168.1.5.5353") {
		t.Errorf("AF_INET (null): unexpected summary: %q", got)
	}
}

// TestSummarizeLoopBigEndian is the regression test for the gn-openbsd.pcap
// finding: OpenBSD's DLT_LOOP framing is network (big-endian) byte order,
// NOT host order like linktypeNull — parsing it with the wrong endianness
// turns AF_INET (2) into 0x02000000 and misses every packet.
func TestSummarizeLoopBigEndian(t *testing.T) {
	got := summarizePacket(linktypeLoop, loopFramedBE(2, minimalIPv4UDP()))
	if !strings.Contains(got, "UDP 192.168.1.5.5353") {
		t.Fatalf("AF_INET (loop, be): unexpected summary: %q", got)
	}
	for _, af := range []uint32{24, 28, 30} {
		got := summarizePacket(linktypeLoop, loopFramedBE(af, minimalIPv6UDP()))
		if !strings.Contains(got, "UDP") || !strings.Contains(got, "fd00:203::1.5353") {
			t.Errorf("AF_INET6=%d (loop, be): unexpected summary: %q", af, got)
		}
	}
	// Same bytes read as host/little-endian (i.e. what the pre-fix code did
	// by treating this framing as plain linktypeNull) must NOT decode —
	// proving endianness, not just the value set, actually matters here.
	if got := summarizePacket(linktypeNull, loopFramedBE(2, minimalIPv4UDP())); strings.Contains(got, "UDP") {
		t.Errorf("big-endian AF_INET bytes decoded as IP under host-order parsing (should not have): %q", got)
	}
}

// TestReconcileLinktype covers the correction at the heart of the FreeBSD/
// macOS fix: BIOCGDLT reporting Ethernet for gravinet's own TUN interface
// (confirmed against two real captures, both mislabeled linktype=1 with
// every packet actually 4-byte-AF-prefixed) gets corrected to linktypeNull,
// but only when the interface has no genuine 6-byte hardware address — a
// real NIC's real Ethernet report must survive untouched.
func TestReconcileLinktype(t *testing.T) {
	tunIface := &net.Interface{Name: "mesh0"} // no HardwareAddr: exactly what a TUN device looks like
	nicIface := &net.Interface{Name: "em0", HardwareAddr: net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}}

	cases := []struct {
		name     string
		ifi      *net.Interface
		reported int
		want     int
	}{
		{"TUN misreported as Ethernet -> corrected to Null", tunIface, linktypeEthernet, linktypeNull},
		{"TUN correctly reported as Null -> unchanged", tunIface, linktypeNull, linktypeNull},
		{"TUN correctly reported as Loop -> unchanged", tunIface, linktypeLoop, linktypeLoop},
		{"TUN with no platform report (-1) -> falls back to guess (Raw)", tunIface, -1, linktypeRaw},
		{"real NIC reported as Ethernet -> trusted as-is", nicIface, linktypeEthernet, linktypeEthernet},
		{"real NIC with no platform report (-1) -> falls back to guess (Ethernet)", nicIface, -1, linktypeEthernet},
	}
	for _, c := range cases {
		if got := reconcileLinktype(c.ifi, c.reported); got != c.want {
			t.Errorf("%s: reconcileLinktype = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestWritePcapPreservesLinktypeNull is the regression test for the
// ambiguity writePcap's old "if linktype == 0" fallback created the moment
// linktypeNull (also numerically 0) became a real, reachable value: a
// genuine BSD/macOS Null-framed capture must be written with linktype 0 in
// the file, not silently promoted to Ethernet. A captureState that never had
// begin() called on it at all — the actual "nothing set yet" case the
// fallback exists for — must still default sanely.
func TestWritePcapPreservesLinktypeNull(t *testing.T) {
	cs := newCaptureState()
	ep, _ := cs.begin("utun3", linktypeRaw) // pre-capture guess
	cs.setLinktype(ep, linktypeNull)        // platform backend's (corrected) report
	cs.addEpoch(ep, time.Now(), nullFramedLE(2, minimalIPv4UDP()))

	var buf bytes.Buffer
	cs.writePcap(&buf)
	gotLinktype := binary.LittleEndian.Uint32(buf.Bytes()[20:24])
	if gotLinktype != linktypeNull {
		t.Errorf("pcap header linktype = %d, want %d (linktypeNull) — a real Null capture must not be coerced to Ethernet", gotLinktype, linktypeNull)
	}
}

func TestWritePcapNeverStartedDefaultsToEthernet(t *testing.T) {
	cs := newCaptureState() // begin() never called
	var buf bytes.Buffer
	cs.writePcap(&buf)
	gotLinktype := binary.LittleEndian.Uint32(buf.Bytes()[20:24])
	if gotLinktype != linktypeEthernet {
		t.Errorf("pcap header linktype = %d, want %d (linktypeEthernet default for a never-started capture)", gotLinktype, linktypeEthernet)
	}
}
