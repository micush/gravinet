package mesh

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/crypto"
)

// TestRemoveNetworkClearsHostsBlock proves that disabling/removing a network
// live (RemoveNetwork, no daemon restart) clears its gravinet-managed hosts
// block immediately. Before this fix, only clearStaleHostsBlocks (run at
// process startup/shutdown) ever removed a network's block, so a network
// disabled via a live config reload left its overlay hostnames stuck in the
// OS hosts file until the whole daemon was stopped and started again.
func TestRemoveNetworkClearsHostsBlock(t *testing.T) {
	const netID = uint64(0x9999)
	dir := t.TempDir()
	hp := filepath.Join(dir, "hosts")
	tag := fmt.Sprintf("%016x", netID)
	stale := "127.0.0.1 localhost\n" +
		"# BEGIN gravinet " + tag + "\n" +
		"10.44.0.7 gnpeer\n" +
		"# END gravinet " + tag + "\n"
	if err := os.WriteFile(hp, []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}

	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev("H")
	eng := NewEngine(Options{NodeID: "H", Hostname: "H"})
	spec := NetSpec{
		ID:        netID,
		Name:      "n",
		Keys:      ks,
		Dev:       dev,
		Self4:     netip.MustParseAddr("10.44.0.1"),
		HostsSync: true,
		HostsPath: hp,
	}
	if err := eng.AddNetwork(spec); err != nil {
		t.Fatalf("AddNetwork: %v", err)
	}
	// Remove it live immediately — well inside maintLoop's tick interval — so
	// this exercises RemoveNetwork's own cleanup, not a periodic syncHosts pass.
	if err := eng.RemoveNetwork(netID); err != nil {
		t.Fatalf("RemoveNetwork: %v", err)
	}

	out, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "10.44.0.7") || strings.Contains(got, "gnpeer") {
		t.Fatalf("stale hosts block not cleared by live RemoveNetwork:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 localhost") {
		t.Fatalf("unrelated host line was lost:\n%s", got)
	}
}
