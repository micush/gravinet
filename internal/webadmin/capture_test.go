package webadmin

import (
	"bytes"
	"encoding/binary"
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
