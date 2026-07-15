package transport

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
)

// TestTLSExtraPortAcceptAndReply verifies that a configured extra TCP/TLS
// port (a) accepts connections and delivers frames, and (b) that a reply to
// that remote goes back down the connection actually dialed — the TCP analog
// of TestExtraPortListenAndReply for UDP. There's nothing to check here about
// *which local port* a reply egresses from the way the UDP test does: Send
// always writes to the specific registered net.Conn for a remote regardless
// of which listener accepted it, so this is really confirming the extra
// listener's accepted connections join the same conns map and Handler path
// the primary listener's do.
func TestTLSExtraPortAcceptAndReply(t *testing.T) {
	primary := freeTCPPort(t)
	extra := freeTCPPort(t)

	srvRec := newRecorder()
	srv, err := OpenTLS(TLSOptions{BindAddr: "127.0.0.1", Port: primary, ExtraPorts: []int{extra}, Handler: srvRec.handle})
	if err != nil {
		t.Fatalf("OpenTLS: %v", err)
	}
	defer srv.Close()
	srvRec.reply = srv // echo "pong" to whoever sends "ping"

	cliRec := newRecorder()
	cli := openTLSLoopback(t, cliRec.handle)
	defer cli.Close()

	// Dial the EXTRA port directly — a real peer given this as a seed address
	// would connect the same way.
	extraAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(extra))
	if err := cli.Dial(extraAddr); err != nil {
		t.Fatalf("dial extra port: %v", err)
	}
	if err := cli.Send(extraAddr, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, srvRec.gotCh, "ping") // received on the extra listener
	waitFor(t, cliRec.gotCh, "pong") // reply arrived back over the same conn
}

// TestTLSExtraPortBadPortSkipped confirms an unbindable extra port (already
// in use) is skipped rather than failing OpenTLS entirely — the same
// best-effort contract the UDP side (transport.Options.ExtraPorts) already
// has, and the one this was deliberately modeled on.
func TestTLSExtraPortBadPortSkipped(t *testing.T) {
	primary := freeTCPPort(t)
	taken := freeTCPPort(t)
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(taken)))
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blocker.Close()

	tr, err := OpenTLS(TLSOptions{
		BindAddr: "127.0.0.1", Port: primary, ExtraPorts: []int{taken},
		Handler: func([]byte, netip.AddrPort, Family) {},
	})
	if err != nil {
		t.Fatalf("open should succeed despite an unbindable extra port: %v", err)
	}
	defer tr.Close()
	if len(tr.extraLns) != 0 {
		t.Fatalf("expected the taken extra port to be skipped, got %d extra listener(s)", len(tr.extraLns))
	}
}

// TestTLSPrimaryPortBusySkipsToExtra confirms an unbindable *primary* port
// (already in use) no longer aborts OpenTLS entirely — it's now best-effort
// exactly like ExtraPorts, so a busy primary still leaves a configured extra
// port listening and serving frames.
func TestTLSPrimaryPortBusySkipsToExtra(t *testing.T) {
	taken := freeTCPPort(t)
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(taken)))
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blocker.Close()
	extra := freeTCPPort(t)

	srvRec := newRecorder()
	srv, err := OpenTLS(TLSOptions{BindAddr: "127.0.0.1", Port: taken, ExtraPorts: []int{extra}, Handler: srvRec.handle})
	if err != nil {
		t.Fatalf("open should succeed on the extra port despite a busy primary: %v", err)
	}
	defer srv.Close()
	if srv.HasPrimary() {
		t.Fatalf("expected the busy primary port to be skipped, not bound")
	}
	if len(srv.extraLns) != 1 {
		t.Fatalf("expected the extra port to be bound despite the busy primary, got %d extra listener(s)", len(srv.extraLns))
	}
	srvRec.reply = srv

	cliRec := newRecorder()
	cli := openTLSLoopback(t, cliRec.handle)
	defer cli.Close()

	extraAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(extra))
	if err := cli.Dial(extraAddr); err != nil {
		t.Fatalf("dial extra port: %v", err)
	}
	if err := cli.Send(extraAddr, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, srvRec.gotCh, "ping") // received despite the primary never having bound
	waitFor(t, cliRec.gotCh, "pong")
}

// TestTLSAllPortsBusyFails confirms OpenTLS still returns an error when
// *nothing* configured could bind — primary and every extra port all already
// taken — rather than silently returning a transport with no listeners at all.
func TestTLSAllPortsBusyFails(t *testing.T) {
	takenPrimary := freeTCPPort(t)
	blockerPrimary, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(takenPrimary)))
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blockerPrimary.Close()
	takenExtra := freeTCPPort(t)
	blockerExtra, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(takenExtra)))
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	defer blockerExtra.Close()

	tr, err := OpenTLS(TLSOptions{
		BindAddr: "127.0.0.1", Port: takenPrimary, ExtraPorts: []int{takenExtra},
		Handler: func([]byte, netip.AddrPort, Family) {},
	})
	if err == nil {
		tr.Close()
		t.Fatalf("expected an error when neither the primary nor any extra port could bind")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}
