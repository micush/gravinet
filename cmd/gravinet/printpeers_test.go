package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// TestPrintPeersRendersAllFields checks printPeers' actual stdout against
// synthetic PeerInfo values covering both the freshly-added fields (key,
// session age, MTU, fragment counts, health) and the pre-existing ones
// (reach, transport, byte counters), plus the zero-value cases a real
// not-yet-probed peer would show (no key label, no established time, MTU
// still probing, no fragment loss yet).
func TestPrintPeersRendersAllFields(t *testing.T) {
	now := time.Now()
	peers := []mesh.PeerInfo{
		{
			NodeID: "node-a", Hostname: "host-a",
			Overlay4: "10.5.0.2", Endpoint: "203.0.113.9:51820",
			Relayed: false, Transport: "udp",
			KeyLabel:      "prod-2026",
			EstablishedAt: now.Add(-90 * time.Minute).UnixNano(),
			PathMTU:       1332,
			FragsSent:     40, FragsRcvd: 38,
			FragSendDrop: 2, ReasmDrop: 0,
			TxBytes: 1_258_291, RxBytes: 50_659_123,
		},
		{
			// A freshly-established, healthy, not-yet-fragmented peer: every
			// optional field at its zero value. This is what a brand new
			// session looks like and must render as "-"/"probing"/"clean",
			// never as a blank column or a zero that reads like real data.
			NodeID: "node-b", Hostname: "host-b",
			Overlay4: "10.5.0.3", Endpoint: "203.0.113.10:51820",
			Relayed: true, // Transport left "" on purpose: pre-v556 daemon
		},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printPeers(peers)
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()
	t.Logf("rendered:\n%s", out)

	for _, want := range []string{
		"node-a", "prod-2026", "1h 30m", "mtu=1332B",
		"frags-tx=40", "frags-rx=38", "drops 2/0",
		"bytes-tx=1.2M", "bytes-rx=48.3M",
		"(direct/udp)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	for _, want := range []string{
		"node-b", "key=-", "up=-", "mtu=probing",
		"frags-tx=0", "frags-rx=0", "clean",
		"bytes-tx=0B", "bytes-rx=0B",
		"(relayed/udp)", // Transport falls back to udp when the daemon predates the field
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}
