package transport

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

// recorder is a handler that captures payloads and can auto-reply.
type recorder struct {
	mu    sync.Mutex
	got   [][]byte
	gotCh chan []byte
	reply *TLSTransport // if set, echo "pong" back to the sender for "ping"
}

func newRecorder() *recorder { return &recorder{gotCh: make(chan []byte, 8)} }

func (r *recorder) handle(p []byte, from netip.AddrPort, _ Family) {
	cp := append([]byte(nil), p...)
	r.mu.Lock()
	r.got = append(r.got, cp)
	r.mu.Unlock()
	select {
	case r.gotCh <- cp:
	default:
	}
	if r.reply != nil && string(cp) == "ping" {
		_ = r.reply.Send(from, []byte("pong"))
	}
}

func waitFor(t *testing.T, ch chan []byte, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func openTLSLoopback(t *testing.T, h Handler) *TLSTransport {
	t.Helper()
	tr, err := OpenTLS(TLSOptions{BindAddr: "127.0.0.1", Port: 0, Handler: h})
	if err != nil {
		t.Fatalf("OpenTLS: %v", err)
	}
	return tr
}

// TestTLSSendReceiveAndReply checks that a dialed TLS connection carries a frame
// to the listener's handler, and that the listener can reply back over the very
// same connection (the reply-routing the engine relies on).
func TestTLSSendReceiveAndReply(t *testing.T) {
	srvRec := newRecorder()
	srv := openTLSLoopback(t, srvRec.handle)
	defer srv.Close()
	srvRec.reply = srv // server echoes "pong" to whoever sent "ping"

	cliRec := newRecorder()
	cli := openTLSLoopback(t, cliRec.handle)
	defer cli.Close()

	srvAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srv.Port()))

	if cli.HasConn(srvAddr) {
		t.Fatal("HasConn true before dial")
	}
	if err := cli.Dial(srvAddr); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if !cli.HasConn(srvAddr) {
		t.Fatal("HasConn false after dial")
	}
	if err := cli.Send(srvAddr, []byte("ping")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, srvRec.gotCh, "ping") // server received over TLS
	waitFor(t, cliRec.gotCh, "pong") // client received the reply over the same conn
}

// TestTLSSendWithoutConn confirms Send fails (rather than dialing) when no
// connection exists, so the data path stays non-blocking and falls back to UDP.
func TestTLSSendWithoutConn(t *testing.T) {
	tr := openTLSLoopback(t, func([]byte, netip.AddrPort, Family) {})
	defer tr.Close()
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 9)
	if err := tr.Send(dst, []byte("x")); err == nil {
		t.Fatal("expected error sending with no conn, got nil")
	}
}

// TestDualPrefersTLSWhenConnExists checks the combined sender: it routes over
// TLS when a connection to the destination exists, and over UDP otherwise.
func TestDualPrefersTLSWhenConnExists(t *testing.T) {
	// TLS server that records what it receives.
	srvRec := newRecorder()
	srv := openTLSLoopback(t, srvRec.handle)
	defer srv.Close()
	srvAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srv.Port()))

	// Client TLS transport with a live conn to the server.
	cliTLS := openTLSLoopback(t, func([]byte, netip.AddrPort, Family) {})
	defer cliTLS.Close()
	if err := cliTLS.Dial(srvAddr); err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// A UDP transport for the fallback leg.
	udp, err := Open(Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: func([]byte, netip.AddrPort, Family) {}})
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer udp.Close()

	d := Dual{UDP: udp, TLS: cliTLS}
	if err := d.Send(srvAddr, []byte("ping")); err != nil {
		t.Fatalf("dual send: %v", err)
	}
	waitFor(t, srvRec.gotCh, "ping") // arrived via TLS, not UDP
}

// TestDualFallsBackToUDP confirms that with no TLS conn (or TLS nil) the dual
// sender uses UDP.
func TestDualFallsBackToUDP(t *testing.T) {
	udpRec := newRecorder()
	recv, err := Open(Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: udpRec.handle})
	if err != nil {
		t.Fatalf("open recv: %v", err)
	}
	defer recv.Close()
	send, err := Open(Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: func([]byte, netip.AddrPort, Family) {}})
	if err != nil {
		t.Fatalf("open send: %v", err)
	}
	defer send.Close()

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	d := Dual{UDP: send, TLS: nil} // TLS unavailable → UDP only
	if err := d.Send(dst, []byte("udp-hello")); err != nil {
		t.Fatalf("dual send: %v", err)
	}
	waitFor(t, udpRec.gotCh, "udp-hello")
}

// TestDualFallbackDial exercises the engine-facing fallback API on Dual:
// HasFallback/DialFallback open a TLS path, after which Send routes over it.
func TestDualFallbackDial(t *testing.T) {
	srvRec := newRecorder()
	srv := openTLSLoopback(t, srvRec.handle)
	defer srv.Close()
	srvAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srv.Port()))

	cliTLS := openTLSLoopback(t, func([]byte, netip.AddrPort, Family) {})
	defer cliTLS.Close()
	udp, err := Open(Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: func([]byte, netip.AddrPort, Family) {}})
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer udp.Close()

	d := Dual{UDP: udp, TLS: cliTLS}
	if d.HasFallback(srvAddr) {
		t.Fatal("HasFallback true before dial")
	}
	if err := d.DialFallback(srvAddr); err != nil {
		t.Fatalf("DialFallback: %v", err)
	}
	if !d.HasFallback(srvAddr) {
		t.Fatal("HasFallback false after dial")
	}
	if err := d.Send(srvAddr, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, srvRec.gotCh, "ping")
}

// TestDualNoFallbackWhenTLSNil confirms the fallback API degrades safely when
// the TLS listener never came up.
func TestDualNoFallbackWhenTLSNil(t *testing.T) {
	udp, err := Open(Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: func([]byte, netip.AddrPort, Family) {}})
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer udp.Close()
	d := Dual{UDP: udp, TLS: nil}
	addr := netip.MustParseAddrPort("127.0.0.1:443")
	if d.HasFallback(addr) {
		t.Fatal("HasFallback should be false with nil TLS")
	}
	if err := d.DialFallback(addr); err == nil {
		t.Fatal("expected error dialing fallback with nil TLS")
	}
}
