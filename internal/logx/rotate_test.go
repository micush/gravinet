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
