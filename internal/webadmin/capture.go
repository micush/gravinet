package webadmin

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Packet capture is a tcpdump-equivalent diagnostic for the authenticated admin
// panel: it opens a raw socket on a chosen interface (Linux AF_PACKET, no libpcap
// — pure stdlib), keeps a bounded rolling buffer of recent packets, shows live
// one-line summaries in the UI, and can export the buffer as a standard .pcap.
//
// It is read-only (no injection) and requires root/CAP_NET_RAW, which the daemon
// already holds for the TUN devices. Because it can observe all traffic on an
// interface, it is gated behind the same admin authentication as the rest of the
// panel and is off until explicitly started.

const (
	capSnaplen    = 262144   // per-packet capture cap (256 KiB)
	capMaxPackets = 5000     // rolling buffer depth
	capMaxBytes   = 32 << 20 // rolling buffer size cap (32 MiB)

	linktypeEthernet = 1   // LINKTYPE_ETHERNET
	linktypeRaw      = 101 // LINKTYPE_RAW (bare IPv4/IPv6, e.g. TUN)
	linktypeNull     = 0   // LINKTYPE_NULL (4-byte address-family header; BSD loopback/utun)
)

// capHandle is the platform capture loop; stop() ends it and frees the socket.
type capHandle interface{ stop() }

type capPacket struct {
	seq     int64
	t       time.Time
	data    []byte
	origlen int
	summary string
}

// captureState is the single active capture owned by the Server.
type captureState struct {
	mu       sync.Mutex
	buf      []capPacket
	bytes    int
	seq      int64
	epoch    int64 // bumped on every start/stop so stray packets from a prior socket are dropped
	running  bool
	iface    string
	linktype int
	handle   capHandle
}

func newCaptureState() *captureState { return &captureState{} }

// addEpoch appends a packet captured under epoch ep, ignoring it if a newer
// capture has since started (ep != current epoch).
func (cs *captureState) addEpoch(ep int64, t time.Time, data []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if ep != cs.epoch {
		return
	}
	cs.seq++
	cs.buf = append(cs.buf, capPacket{
		seq: cs.seq, t: t, data: data, origlen: len(data),
		summary: summarizePacket(cs.linktype, data),
	})
	cs.bytes += len(data)
	drop := 0
	for (len(cs.buf)-drop) > capMaxPackets || cs.bytes > capMaxBytes {
		cs.bytes -= len(cs.buf[drop].data)
		drop++
	}
	if drop > 0 {
		n := copy(cs.buf, cs.buf[drop:])
		for i := n; i < len(cs.buf); i++ {
			cs.buf[i] = capPacket{} // release references to dropped packets
		}
		cs.buf = cs.buf[:n]
	}
}

// begin stops any running capture, resets the buffer, and starts a new one.
func (cs *captureState) begin(ifaceName string, linktype int) (int64, capHandle) {
	cs.mu.Lock()
	old := cs.handle
	cs.epoch++
	cs.handle = nil
	cs.buf = nil
	cs.bytes = 0
	cs.iface = ifaceName
	cs.linktype = linktype
	cs.running = true
	ep := cs.epoch
	cs.mu.Unlock()
	if old != nil {
		old.stop()
	}
	return ep, nil
}

// setLinktype overrides the guessed linktype with one a platform backend
// discovered directly (e.g. via BIOCGDLT on macOS or pcap_datalink() with
// Npcap on Windows), as long as ep is still the current capture.
func (cs *captureState) setLinktype(ep int64, lt int) {
	cs.mu.Lock()
	if cs.epoch == ep {
		cs.linktype = lt
	}
	cs.mu.Unlock()
}

func (cs *captureState) setHandle(ep int64, h capHandle) {
	cs.mu.Lock()
	if cs.epoch == ep {
		cs.handle = h
		cs.mu.Unlock()
		return
	}
	cs.mu.Unlock()
	h.stop() // a newer capture superseded this one before it finished starting
}

func (cs *captureState) failStart(ep int64) {
	cs.mu.Lock()
	if cs.epoch == ep {
		cs.running = false
	}
	cs.mu.Unlock()
}

func (cs *captureState) stop() {
	cs.mu.Lock()
	old := cs.handle
	cs.handle = nil
	cs.running = false
	cs.epoch++
	cs.mu.Unlock()
	if old != nil {
		old.stop()
	}
}

func (cs *captureState) clear() {
	cs.mu.Lock()
	cs.buf = nil
	cs.bytes = 0
	cs.mu.Unlock()
}

// since returns packets newer than the given seq (capped to the most recent max),
// along with the current cursor and capture status.
func (cs *captureState) since(after int64, max int) (pkts []capPacket, cursor int64, running bool, iface string, linktype int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cursor, running, iface, linktype = cs.seq, cs.running, cs.iface, cs.linktype
	for _, p := range cs.buf {
		if p.seq > after {
			pkts = append(pkts, p)
		}
	}
	if len(pkts) > max {
		pkts = pkts[len(pkts)-max:]
	}
	return
}

func (cs *captureState) writePcap(w io.Writer) {
	cs.mu.Lock()
	linktype := cs.linktype
	pkts := make([]capPacket, len(cs.buf))
	copy(pkts, cs.buf)
	cs.mu.Unlock()
	if linktype == 0 {
		linktype = linktypeEthernet
	}
	w.Write(pcapGlobalHeader(capSnaplen, linktype))
	for _, p := range pkts {
		w.Write(pcapRecord(p.t, len(p.data), p.origlen, p.data))
	}
}

// linktypeForIface chooses the pcap link type for an interface. Ethernet-framed
// interfaces (and loopback, which AF_PACKET frames as Ethernet) use ETHERNET;
// point-to-point/TUN interfaces with no hardware address deliver bare IP packets.
func linktypeForIface(ifi *net.Interface) int {
	if ifi.Flags&net.FlagLoopback != 0 || len(ifi.HardwareAddr) == 6 {
		return linktypeEthernet
	}
	return linktypeRaw
}

// ---- pcap format (classic libpcap, microsecond, little-endian) --------------

func pcapGlobalHeader(snaplen, linktype int) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(b[4:], 2)
	binary.LittleEndian.PutUint16(b[6:], 4)
	// thiszone (8:12) and sigfigs (12:16) are 0
	binary.LittleEndian.PutUint32(b[16:], uint32(snaplen))
	binary.LittleEndian.PutUint32(b[20:], uint32(linktype))
	return b
}

func pcapRecord(t time.Time, caplen, origlen int, data []byte) []byte {
	b := make([]byte, 16+len(data))
	binary.LittleEndian.PutUint32(b[0:], uint32(t.Unix()))
	binary.LittleEndian.PutUint32(b[4:], uint32(t.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(b[8:], uint32(caplen))
	binary.LittleEndian.PutUint32(b[12:], uint32(origlen))
	copy(b[16:], data)
	return b
}

// ---- one-line packet summaries (tcpdump-ish) --------------------------------

func summarizePacket(linktype int, data []byte) string {
	etype := 0
	off := 0
	if linktype == linktypeEthernet {
		if len(data) < 14 {
			return fmt.Sprintf("short frame (%d bytes)", len(data))
		}
		etype = int(binary.BigEndian.Uint16(data[12:14]))
		off = 14
		if etype == 0x8100 && len(data) >= 18 { // VLAN tag
			etype = int(binary.BigEndian.Uint16(data[16:18]))
			off = 18
		}
	} else if linktype == linktypeNull {
		// BSD loopback/utun framing: a 4-byte address family in host byte
		// order precedes the raw IP packet. macOS/BSD are little-endian on
		// every currently-shipping architecture (Intel and Apple Silicon).
		if len(data) < 4 {
			return fmt.Sprintf("short frame (%d bytes)", len(data))
		}
		switch binary.LittleEndian.Uint32(data[0:4]) {
		case 2: // AF_INET
			etype = 0x0800
		case 30: // AF_INET6 (macOS/BSD value; differs from Linux's 10)
			etype = 0x86DD
		default:
			return fmt.Sprintf("non-IP (%d bytes)", len(data))
		}
		off = 4
	} else {
		if len(data) < 1 {
			return "empty"
		}
		switch data[0] >> 4 {
		case 4:
			etype = 0x0800
		case 6:
			etype = 0x86DD
		default:
			return fmt.Sprintf("non-IP (%d bytes)", len(data))
		}
	}
	switch etype {
	case 0x0800:
		return summarizeIPv4(data[off:], len(data))
	case 0x86DD:
		return summarizeIPv6(data[off:], len(data))
	case 0x0806:
		return fmt.Sprintf("ARP, length %d", len(data))
	default:
		return fmt.Sprintf("ethertype 0x%04x, length %d", etype, len(data))
	}
}

func summarizeIPv4(p []byte, total int) string {
	if len(p) < 20 {
		return "truncated IPv4"
	}
	ihl := int(p[0]&0x0f) * 4
	if ihl < 20 {
		ihl = 20
	}
	proto := p[9]
	src := net.IP(p[12:16]).String()
	dst := net.IP(p[16:20]).String()
	return l4Summary(proto, src, dst, p, ihl, total)
}

func summarizeIPv6(p []byte, total int) string {
	if len(p) < 40 {
		return "truncated IPv6"
	}
	next := p[6]
	src := net.IP(p[8:24]).String()
	dst := net.IP(p[24:40]).String()
	return l4Summary(next, src, dst, p, 40, total)
}

func l4Summary(proto byte, src, dst string, p []byte, l4off, total int) string {
	switch proto {
	case 6, 17: // TCP, UDP
		name := "UDP"
		if proto == 6 {
			name = "TCP"
		}
		if len(p) >= l4off+4 {
			sport := binary.BigEndian.Uint16(p[l4off : l4off+2])
			dport := binary.BigEndian.Uint16(p[l4off+2 : l4off+4])
			flags := ""
			if proto == 6 && len(p) >= l4off+14 {
				flags = " [" + tcpFlags(p[l4off+13]) + "]"
			}
			return fmt.Sprintf("%s %s.%d > %s.%d%s, length %d", name, src, sport, dst, dport, flags, total)
		}
		return fmt.Sprintf("%s %s > %s, length %d", name, src, dst, total)
	case 1:
		return fmt.Sprintf("ICMP %s > %s, length %d", src, dst, total)
	case 58:
		return fmt.Sprintf("ICMPv6 %s > %s, length %d", src, dst, total)
	default:
		return fmt.Sprintf("IP proto %d %s > %s, length %d", proto, src, dst, total)
	}
}

func tcpFlags(b byte) string {
	out := ""
	for _, f := range []struct {
		bit  byte
		char string
	}{{0x02, "S"}, {0x10, "."}, {0x01, "F"}, {0x04, "R"}, {0x08, "P"}, {0x20, "U"}} {
		if b&f.bit != 0 {
			out += f.char
		}
	}
	if out == "" {
		out = "-"
	}
	return out
}

// ---- HTTP handlers ----------------------------------------------------------

func (s *Server) handleCaptureInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := net.Interfaces()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"interfaces": []any{}})
		return
	}
	type ifInfo struct {
		Name string `json:"name"`
		Up   bool   `json:"up"`
	}
	out := make([]ifInfo, 0, len(ifaces))
	for _, ifi := range ifaces {
		// Loopback carries no mesh traffic and only clutters the list.
		if ifi.Flags&net.FlagLoopback != 0 || ifi.Name == "lo" {
			continue
		}
		out = append(out, ifInfo{Name: ifi.Name, Up: ifi.Flags&net.FlagUp != 0})
	}
	writeJSON(w, http.StatusOK, map[string]any{"interfaces": out, "supported": captureSupported})
}

func (s *Server) handleCaptureStart(w http.ResponseWriter, r *http.Request) {
	var req struct{ Iface string }
	if !decode(w, r, &req) {
		return
	}
	ifi, err := net.InterfaceByName(req.Iface)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown interface"})
		return
	}
	ep, _ := s.capture.begin(ifi.Name, linktypeForIface(ifi))
	h, lt, err := startCapture(ifi.Name, capSnaplen, func(t time.Time, d []byte) {
		s.capture.addEpoch(ep, t, d)
	})
	if err != nil {
		s.capture.failStart(ep)
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	if lt >= 0 { // a platform backend may report the linktype it actually found
		s.capture.setLinktype(ep, lt)
	}
	s.capture.setHandle(ep, h)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "iface": ifi.Name})
}

func (s *Server) handleCaptureStop(w http.ResponseWriter, r *http.Request) {
	s.capture.stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCaptureClear(w http.ResponseWriter, r *http.Request) {
	s.capture.clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCapturePackets(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	pkts, cursor, running, iface, _ := s.capture.since(after, 3000)
	type row struct {
		Seq     int64  `json:"seq"`
		Time    string `json:"time"`
		Summary string `json:"summary"`
	}
	rows := make([]row, 0, len(pkts))
	for _, p := range pkts {
		rows = append(rows, row{Seq: p.seq, Time: p.t.Format("15:04:05.000"), Summary: p.summary})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running": running, "iface": iface, "cursor": cursor,
		"supported": captureSupported, "packets": rows,
	})
}

func (s *Server) handleCapturePcap(w http.ResponseWriter, r *http.Request) {
	s.capture.mu.Lock()
	iface := s.capture.iface
	s.capture.mu.Unlock()
	if iface == "" {
		iface = "capture"
	}
	name := fmt.Sprintf("gravinet-%s-%s.pcap", iface, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	s.capture.writePcap(w)
}
