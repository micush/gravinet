package logx

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// failingWriter always errors, simulating os.Stderr under a Windows service
// (no console attached => every write to it fails).
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated broken console")
}

func TestBestEffortDoesNotBlockMultiWriter(t *testing.T) {
	var file bytes.Buffer
	// Without BestEffort, io.MultiWriter would stop at the first error and
	// never reach `file` — which is exactly the bug that made log files
	// empty when gravinet ran as a Windows service.
	w := io.MultiWriter(BestEffort(failingWriter{}), &file)

	msg := []byte("hello\n")
	n, err := w.Write(msg)
	if err != nil {
		t.Fatalf("Write returned error %v, want nil (BestEffort should swallow the failing writer's error)", err)
	}
	if n != len(msg) {
		t.Fatalf("Write returned n=%d, want %d", n, len(msg))
	}
	if file.String() != "hello\n" {
		t.Fatalf("file content = %q, want %q — the real writer must still receive the data", file.String(), "hello\n")
	}
}

func TestBestEffortPassesThroughSuccessfulWrites(t *testing.T) {
	var buf bytes.Buffer
	w := BestEffort(&buf)
	n, err := w.Write([]byte("ok"))
	if err != nil || n != 2 {
		t.Fatalf("Write() = (%d, %v), want (2, nil)", n, err)
	}
	if buf.String() != "ok" {
		t.Fatalf("buf = %q, want %q", buf.String(), "ok")
	}
}
