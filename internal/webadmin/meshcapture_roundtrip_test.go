package webadmin

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// TestCaptureOnePeerReportsIfaceBeforeDeadline is a direct regression test
// for a real bug: the Capture all mesh peers table showed "iface: -" for
// every peer for the entire capture window, not just before discovery
// finished. The cause was that captureOnePeer's caller (meshCaptureJob.run)
// only wrote res.Iface from captureOnePeer's return value — and
// captureOnePeer doesn't return until *after* it has slept all the way to
// the shared deadline. The interface name is actually known within the
// first HTTP round trip, well before that sleep even starts; it just never
// got published early. setIface (job.run's callback, invoked mid-function
// now) is the fix — this test proves it fires close to when the capture
// starts, not close to when it stops.
//
// A bare httptest mux stands in for peer B rather than a full second
// webadmin Server (contrast TestProxyRoutesToCorrectPeer in
// proxy_roundtrip_test.go): captureOnePeer's remote leg only ever calls
// capture's own four endpoints, and none of gravinet's own /api/capture/*
// handlers can run in this sandbox anyway (they need a real raw-socket
// interface named "mesh0", which doesn't exist here) — so faking exactly
// those four responses tests captureOnePeer itself precisely, without also
// depending on packet-capture support existing on the test machine.
func TestCaptureOnePeerReportsIfaceBeforeDeadline(t *testing.T) {
	var mu sync.Mutex
	var startedAt, ifaceAt, stoppedAt time.Time

	mux := http.NewServeMux()
	mux.HandleFunc("/api/capture/mesh-iface", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ifaces":[{"network":"lan","iface":"mesh0"}]}`))
	})
	mux.HandleFunc("/api/capture/start", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		startedAt = time.Now()
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/capture/stop", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stoppedAt = time.Now()
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/capture/pcap", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-pcap-bytes"))
	})
	peer := httptest.NewTLSServer(mux) // TLS: captureOnePeer always dials "https://", same as handleProxy
	defer peer.Close()

	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	peerPort, err := strconv.Atoi(peerURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	srv, be, _ := newTestServer(t)
	be.overlayAddr = netip.MustParseAddr("127.0.0.1") // so resolveManagedTarget's SSRF guard (OverlayContains) accepts the loopback stand-in
	be.managedPeers = []mesh.ManagedPeer{{
		NodeID: "peer-b", Hostname: "gn-peerb",
		Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: uint16(peerPort),
		LastSeen: time.Now(), Connected: true,
	}}

	const window = 2 * time.Second
	start := time.Now()
	deadline := start.Add(window)

	data, iface, err := captureOnePeer(srv, "peer-b", false, deadline, func(ifc string) {
		mu.Lock()
		ifaceAt = time.Now()
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("captureOnePeer: %v", err)
	}
	if iface != "mesh0" {
		t.Errorf("iface = %q, want mesh0", iface)
	}
	if string(data) != "fake-pcap-bytes" {
		t.Errorf("data = %q, want fake-pcap-bytes", data)
	}

	mu.Lock()
	defer mu.Unlock()
	if ifaceAt.IsZero() {
		t.Fatal("setIface was never called")
	}
	if startedAt.IsZero() || stoppedAt.IsZero() {
		t.Fatal("the peer's start or stop endpoint was never called")
	}

	toIface := ifaceAt.Sub(start)
	toStop := stoppedAt.Sub(start)
	// The actual regression: setIface has to land soon after the capture
	// starts, not sit until just before stop (which only happens once the
	// full window has elapsed) — that's exactly what left the status table
	// showing "-" the entire time.
	if toIface > window/2 {
		t.Errorf("setIface fired %v into a %v window, only %v before stop — iface is still only reported near the end, the exact bug this fixes", toIface, window, toStop-toIface)
	}
	if toStop < window-200*time.Millisecond {
		t.Errorf("stop happened only %v after start, want roughly the full %v window", toStop, window)
	}
}
