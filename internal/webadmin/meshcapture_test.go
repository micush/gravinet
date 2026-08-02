package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

// readTgz unpacks archived into a name->content map, for tests to assert
// against without repeating the gzip/tar plumbing each time.
func readTgz(t *testing.T, archived []byte) map[string]string {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(archived))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	tr := tar.NewReader(gzr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading member %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(b)
	}
	return out
}

// TestMeshCaptureBundleRoundTrips checks the common case: every peer
// succeeds, and each one's pcap bytes come back under a distinct,
// hostname-derived member name.
func TestMeshCaptureBundleRoundTrips(t *testing.T) {
	j := &meshCaptureJob{peers: []*meshCapturePeerResult{
		{Hostname: "gn-router1", Self: true, Status: "done", pcap: []byte("PCAP-LOCAL")},
		{Hostname: "gn-office", NodeID: "n2", Status: "done", pcap: []byte("PCAP-OFFICE")},
	}}
	j.bundle()

	if !j.done {
		t.Fatal("bundle did not mark the job done")
	}
	if j.failed != "" {
		t.Fatalf("unexpected job failure: %s", j.failed)
	}
	if j.tgz == nil {
		t.Fatal("bundle produced no archive despite two successful peers")
	}

	files := readTgz(t, j.tgz)
	if got := files["gn-router1-local.pcap"]; got != "PCAP-LOCAL" {
		t.Errorf("gn-router1-local.pcap = %q, want %q (files: %v)", got, "PCAP-LOCAL", files)
	}
	if got := files["gn-office.pcap"]; got != "PCAP-OFFICE" {
		t.Errorf("gn-office.pcap = %q, want %q (files: %v)", got, "PCAP-OFFICE", files)
	}
	if _, hasErrors := files["errors.txt"]; hasErrors {
		t.Error("errors.txt present despite no peer failures")
	}
}

// TestMeshCaptureBundlePartialFailure checks that one peer erroring doesn't
// drop the others' data, and that the failure shows up in errors.txt rather
// than silently vanishing.
func TestMeshCaptureBundlePartialFailure(t *testing.T) {
	j := &meshCaptureJob{peers: []*meshCapturePeerResult{
		{Hostname: "gn-router1", Self: true, Status: "done", pcap: []byte("PCAP-LOCAL")},
		{Hostname: "gn-laptop", NodeID: "n3", Status: "error", Error: "peer not reachable for management"},
	}}
	j.bundle()

	if j.failed != "" {
		t.Fatalf("job should still succeed overall when at least one peer worked, got failed=%q", j.failed)
	}
	if j.tgz == nil {
		t.Fatal("bundle produced no archive despite one successful peer")
	}

	files := readTgz(t, j.tgz)
	if got := files["gn-router1-local.pcap"]; got != "PCAP-LOCAL" {
		t.Errorf("successful peer's pcap missing/wrong: %q", got)
	}
	errTxt, ok := files["errors.txt"]
	if !ok {
		t.Fatal("errors.txt missing despite a failed peer")
	}
	if !strings.Contains(errTxt, "gn-laptop") || !strings.Contains(errTxt, "peer not reachable for management") {
		t.Errorf("errors.txt doesn't identify the failed peer/reason: %q", errTxt)
	}
}

// TestMeshCaptureBundleAllFail checks the every-peer-failed case reports a
// job-level failure and produces no archive at all, rather than an empty or
// misleading .tgz.
func TestMeshCaptureBundleAllFail(t *testing.T) {
	j := &meshCaptureJob{peers: []*meshCapturePeerResult{
		{Hostname: "gn-router1", Self: true, Status: "error", Error: "packet capture is not supported on this platform"},
	}}
	j.bundle()

	if !j.done {
		t.Fatal("bundle did not mark the job done")
	}
	if j.tgz != nil {
		t.Error("archive produced despite every peer failing")
	}
	if !strings.Contains(j.failed, "not supported on this platform") {
		t.Errorf("job failure = %q, want it to surface the one peer's actual error", j.failed)
	}
}

// TestMeshCaptureBundleNameCollision checks that two peers whose hostnames
// sanitize to the same string (e.g. two nodes both just called "gravinet",
// or names differing only in characters the sanitizer strips) still each get
// their own file instead of one silently overwriting the other.
func TestMeshCaptureBundleNameCollision(t *testing.T) {
	j := &meshCaptureJob{peers: []*meshCapturePeerResult{
		{Hostname: "office node", NodeID: "n1", Status: "done", pcap: []byte("A")},
		{Hostname: "office/node", NodeID: "n2", Status: "done", pcap: []byte("B")},
	}}
	j.bundle()

	files := readTgz(t, j.tgz)
	if len(files) != 2 {
		t.Fatalf("want 2 distinct members for colliding names, got %d: %v", len(files), files)
	}
	got := map[string]bool{}
	for name, content := range files {
		got[content] = true
		if !strings.HasPrefix(name, "office_node") {
			t.Errorf("unexpected member name %q for a sanitized collision", name)
		}
	}
	if !got["A"] || !got["B"] {
		t.Errorf("lost one peer's content in a name collision: %v", files)
	}
}

func TestSleepUntilReturnsImmediatelyForPastDeadline(t *testing.T) {
	// Not a timing precision test (that would be flaky) — just confirms a
	// deadline that has already passed doesn't block, which is exactly what
	// happens to a peer whose start call alone ate the whole capture window.
	done := make(chan struct{})
	go func() {
		sleepUntil(time.Now().Add(-time.Second))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sleepUntil blocked on an already-past deadline")
	}
}
