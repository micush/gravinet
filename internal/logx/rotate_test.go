package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// Rotate every 100 bytes, keep 2 backups.
	r, err := NewRotatingFile(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Each line ~30 bytes; writing 20 lines forces several rotations.
	for i := 0; i < 20; i++ {
		if _, err := r.Write([]byte(fmt.Sprintf("line %02d xxxxxxxxxxxxxxxxxxxx\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()

	// The live file plus at most maxBackups (.1, .2) — never .3.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected backup .1: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf("expected backup .2: %v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error(".3 should have been pruned (maxBackups=2)")
	}

	// No file should exceed the cap by more than one line's worth.
	for _, p := range []string{path, path + ".1", path + ".2"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Size() > 100+40 {
			t.Errorf("%s = %d bytes, expected near the 100-byte cap", p, fi.Size())
		}
	}

	// The newest line must be in the live file (most recent writes).
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "line 19") {
		t.Errorf("live file should hold the newest line; got %q", string(b))
	}
}

func TestRotatingFileNoBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r, err := NewRotatingFile(path, 50, 0) // keep none: truncate on rotate
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r.Write([]byte(fmt.Sprintf("entry-%d-padding-padding\n", i)))
	}
	r.Close()
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("no backups expected when maxBackups=0")
	}
	fi, _ := os.Stat(path)
	if fi.Size() > 50+40 {
		t.Errorf("file should stay near the cap, got %d", fi.Size())
	}
}

func TestRotationThroughLogger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	rf, err := NewRotatingFile(path, 2<<10, 3) // 2 KiB cap, 3 backups
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	lg := New(rf, LevelInfo) // logger writing straight to the rotating file
	for i := 0; i < 200; i++ {
		lg.Infof("event %d: the quick brown fox jumps over the lazy dog", i)
	}
	// Rotation must have produced backups, none beyond the cap count.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("logger writes should have triggered rotation: %v", err)
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error(".4 should not exist (maxBackups=3)")
	}
}

func TestRotatingFileTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf, err := NewRotatingFile(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatal(err)
	}
	if err := rf.Truncate(); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// File is now empty and the size counter reset.
	if fi, _ := os.Stat(path); fi.Size() != 0 {
		t.Fatalf("after truncate file size = %d, want 0", fi.Size())
	}
	// Writing after truncate appends from the start, not the old offset.
	if _, err := rf.Write([]byte("fresh\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "fresh\n" {
		t.Fatalf("after truncate+write, file = %q, want %q", got, "fresh\n")
	}
}

func TestFIFOFileRollingWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r, err := NewFIFOFile(path, 1000) // 1000-byte single-file cap
	if err != nil {
		t.Fatal(err)
	}
	// Write far more than the cap; the file must stay a rolling window of the
	// newest lines, never exceeding the cap and never producing a backup.
	for i := 0; i < 500; i++ {
		if _, err := r.Write([]byte(fmt.Sprintf("line %04d padding padding padding\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("FIFO mode must not create a .1 backup")
	}
	fi, _ := os.Stat(path)
	if fi.Size() > 1000 {
		t.Errorf("file = %d bytes, must not exceed the 1000-byte cap", fi.Size())
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.Contains(s, "line 0499") {
		t.Errorf("newest line must survive; file was:\n%s", s)
	}
	if strings.Contains(s, "line 0000") {
		t.Errorf("oldest line must have been dropped; file was:\n%s", s)
	}
	// The first surviving line must be whole, never a mid-line fragment.
	if first := strings.SplitN(s, "\n", 2)[0]; first != "" && !strings.HasPrefix(first, "line ") {
		t.Errorf("first surviving line is a fragment: %q", first)
	}
}

func TestFIFOFileShrinkLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r, err := NewFIFOFile(path, 100<<10) // start with a big cap
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for i := 0; i < 1000; i++ {
		r.Write([]byte(fmt.Sprintf("line %04d xxxxxxxxxxxxxxxxxxxx\n", i)))
	}
	// Lowering the cap must trim the on-disk file immediately, not only on the
	// next overflowing write (this is what the web admin's live Log Size change
	// relies on).
	r.SetMaxBytes(500)
	if fi, _ := os.Stat(path); fi.Size() > 500 {
		t.Errorf("after live shrink file = %d bytes, want <= 500", fi.Size())
	}
	if got := r.MaxBytes(); got != 500 {
		t.Errorf("MaxBytes() = %d, want 500", got)
	}
}

func TestFIFOFileTrimsOversizedOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// Pre-seed a file larger than the cap we'll open it with.
	var big []byte
	for i := 0; i < 400; i++ {
		big = append(big, []byte(fmt.Sprintf("old line %04d padding padding\n", i))...)
	}
	if err := os.WriteFile(path, big, 0o640); err != nil {
		t.Fatal(err)
	}
	r, err := NewFIFOFile(path, 800)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Opening in FIFO mode with a smaller cap should have trimmed it already.
	if fi, _ := os.Stat(path); fi.Size() > 800 {
		t.Errorf("oversized file not trimmed on open: %d bytes", fi.Size())
	}
}
