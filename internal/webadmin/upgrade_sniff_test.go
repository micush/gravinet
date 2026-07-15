package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"gravinet/internal/upgrade"
)

func TestSniffAndStageRoutesBinaryVsSource(t *testing.T) {
	// A raw "binary" (just needs to not start with the gzip magic bytes for
	// this test's purposes — sniffAndStage only needs to get the routing
	// decision right; stageUnsignedArtifact's own probing is covered by
	// TestSourceUploadPipelineManual-style checks elsewhere against a real
	// binary).
	t.Run("non-gzip content routes to stageUnsignedArtifact and fails there (not a real binary), not in extraction", func(t *testing.T) {
		dir := t.TempDir()
		st, err := upgrade.NewStore(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, builtFromSource, err := sniffAndStage(st, bytes.NewReader([]byte("MZ not a real binary but definitely not gzip either")))
		if builtFromSource {
			t.Fatal("plain bytes without gzip magic must not be routed to the source-build path")
		}
		if err == nil {
			t.Fatal("expected an error (garbage isn't a runnable binary), got nil")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("identify")) {
			t.Fatalf("expected a probe/identify failure, got: %v", err)
		}
	})

	t.Run("gzip content routes to stageFromSource", func(t *testing.T) {
		dir := t.TempDir()
		st, err := upgrade.NewStore(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		content := "module gravinet\n"
		tw.WriteHeader(&tar.Header{Name: "gravinet/go.mod", Mode: 0644, Size: int64(len(content))})
		tw.Write([]byte(content))
		tw.Close()
		gz.Close()

		_, builtFromSource, err := sniffAndStage(st, &buf)
		if !builtFromSource {
			t.Fatal("gzip magic bytes must route to the source-build path")
		}
		// Expected to fail past routing (no cmd/gravinet in this minimal
		// fixture) -- routing correctness is what this test checks, not a
		// full build.
		if err == nil {
			t.Fatal("expected an error (no cmd/gravinet in this minimal fixture), got nil")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("cmd/gravinet")) {
			t.Fatalf("expected the 'no cmd/gravinet' error, got: %v", err)
		}
	})
}
