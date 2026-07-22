package transport

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// TestExtraPortListenAndReply verifies that a configured extra port (a) receives
// datagrams and (b) that a reply to that remote egresses from the extra port, not
// the primary — the property stateful firewalls/NAT require.
func TestExtraPortListenAndReply(t *testing.T) {
	// Pick two free UDP ports for primary + extra.
	primary := freeUDPPort(t)
	extra := freeUDPPort(t)

	var mu sync.Mutex
	var gotFrom netip.AddrPort
	got := make(chan struct{}, 1)
	tr, err := Open(Options{
		BindAddr: "127.0.0.1", PrimaryPort: primary, ExtraPorts: []int{extra},
		EnableV4: true, Workers: 1,
		Handler: func(payload []byte, from netip.AddrPort, fam Family) {
			mu.Lock()
			gotFrom = from
			mu.Unlock()
			select {
			case got <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()

	// A "remote" client socket dials the EXTRA port.
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()
	extraAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: extra}
	if _, err := client.WriteToUDP([]byte("hello"), extraAddr); err != nil {
		t.Fatalf("client write: %v", err)
	}

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not receive on the extra port")
	}

	// Reply to the client; it must arrive FROM the extra port.
	mu.Lock()
	remote := gotFrom
	mu.Unlock()
	if err := tr.Send(remote, []byte("reply")); err != nil {
		t.Fatalf("send: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, srcAP, err := client.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf[:n]) != "reply" {
		t.Fatalf("got %q", buf[:n])
	}
	if int(srcAP.Port()) != extra {
		t.Fatalf("reply came from port %d, want extra port %d (firewall would drop it)", srcAP.Port(), extra)
	}
	if int(srcAP.Port()) == primary {
		t.Fatal("reply came from the primary port, not the arrival port")
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return p
}
