package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gravinet/internal/config"
)

func TestClearStaleHostsBlocks(t *testing.T) {
	dir := t.TempDir()
	hp := filepath.Join(dir, "hosts")
	id := "00000000deadbeef"
	idn, _ := strconv.ParseUint(id, 16, 64)
	tag := fmt.Sprintf("%016x", idn)
	// A leftover managed block mapping a peer hostname to its overlay IP, plus an
	// unrelated user line that must be preserved.
	stale := "127.0.0.1 localhost\n" +
		"# BEGIN gravinet " + tag + "\n" +
		"10.77.42.99 gnpeer\n" +
		"# END gravinet " + tag + "\n"
	if err := os.WriteFile(hp, []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Networks: []config.Network{{
		ID:        id,
		HostsSync: config.HostsSync{Enabled: true, Path: hp},
	}}}
	clearStaleHostsBlocks(cfg)
	out, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "10.77.42.99") || strings.Contains(got, "gnpeer") {
		t.Fatalf("stale overlay entry not cleared:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 localhost") {
		t.Fatalf("unrelated host line was lost:\n%s", got)
	}
}
