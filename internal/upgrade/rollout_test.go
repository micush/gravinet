package upgrade

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fetch ---

// fakeFleetHTTP serves blobs for the sources that "have" the artifact and fails
// for the ones that do not, so the source-failover path can be exercised without
// an overlay.
type fakeFleetHTTP struct {
	mu      sync.Mutex
	body    []byte
	serve   map[string]bool // host -> will serve
	tried   []string
	corrupt map[string]bool // host -> serves the right length, wrong bytes
}

func (f *fakeFleetHTTP) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := req.URL.Hostname()
	f.tried = append(f.tried, host)
	if f.corrupt[host] {
		bad := bytes.Repeat([]byte("X"), len(f.body))
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(bad))}, nil
	}
	if !f.serve[host] {
		return nil, fmt.Errorf("connection refused")
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

func TestFetchFallsBackToAnotherSource(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := NewStore(t.TempDir(), []string{pub})

	dead := netip.MustParseAddr("10.42.0.2")
	liar := netip.MustParseAddr("10.42.0.3")
	good := netip.MustParseAddr("10.42.0.4")
	f := &fakeFleetHTTP{
		body:    body,
		serve:   map[string]bool{good.String(): true, liar.String(): true},
		corrupt: map[string]bool{liar.String(): true},
	}
	// RTT order puts the dead node first and the honest one last, so the test
	// only passes if every source is genuinely tried.
	srcs := []Source{
		{NodeID: "dead", Addr: dead, Port: 8443, RTT: 1 * time.Millisecond},
		{NodeID: "liar", Addr: liar, Port: 8443, RTT: 5 * time.Millisecond},
		{NodeID: "good", Addr: good, Port: 8443, RTT: 9 * time.Millisecond},
	}
	overlay := func(a netip.Addr) bool { return a.Is4() && a.As4()[0] == 10 }

	if err := Fetch(context.Background(), st, m, srcs, f, overlay); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !st.Have(m) {
		t.Fatal("artifact was not stored")
	}
	if len(f.tried) != 3 {
		t.Fatalf("tried %v; every source should have been attempted, nearest first", f.tried)
	}
	// The node that served correctly-sized garbage must not have poisoned the
	// store — the digest check is what makes it safe to pull from any peer.
	if got := probeVersion(t, mustBinPath(t, st, m)); got != "399" {
		t.Fatalf("stored artifact reports %q", got)
	}
}

// An advertised source pointing at something that is not an overlay address is
// the SSRF case: a hostile peer aiming a fetch at 127.0.0.1 or a metadata endpoint.
func TestFetchRefusesNonOverlaySources(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	body, _ := os.ReadFile(bin)
	st, _ := NewStore(t.TempDir(), []string{pub})

	f := &fakeFleetHTTP{body: body, serve: map[string]bool{"169.254.169.254": true, "127.0.0.1": true}}
	srcs := []Source{
		{NodeID: "evil", Addr: netip.MustParseAddr("169.254.169.254"), Port: 80},
		{NodeID: "evil2", Addr: netip.MustParseAddr("127.0.0.1"), Port: 8443},
	}
	overlay := func(a netip.Addr) bool { return a.Is4() && a.As4()[0] == 10 }

	err := Fetch(context.Background(), st, m, srcs, f, overlay)
	if err == nil {
		t.Fatal("a fetch was made to a non-overlay address")
	}
	if len(f.tried) != 0 {
		t.Fatalf("the guard let a dial through to %v", f.tried)
	}
}

func mustBinPath(t *testing.T, st *Store, m Manifest) string {
	t.Helper()
	p, err := st.BinPath(m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- rollout ---

// fakeFleet is ten nodes that can be told to misbehave in the specific ways real
// ones do: refuse the apply, restart and never come back, or come back on the
// old version having reverted themselves.
type fakeFleet struct {
	mu sync.Mutex

	version  map[string]string // node -> version it currently reports
	phase    map[string]Phase
	lastErr  map[string]string
	offline  map[string]bool // not on the mesh
	refuse   map[string]string
	blackout map[string]bool // accepts the apply, then never returns
	reverts  map[string]bool // accepts, restarts, and reverts itself

	applied []string   // order in which nodes were told to apply
	sources [][]Source // the source list each apply was handed
}

func newFakeFleet(nodes []Target, from string) *fakeFleet {
	f := &fakeFleet{
		version: map[string]string{}, phase: map[string]Phase{}, lastErr: map[string]string{},
		offline: map[string]bool{}, refuse: map[string]string{},
		blackout: map[string]bool{}, reverts: map[string]bool{},
	}
	for _, t := range nodes {
		f.version[t.NodeID] = from
		f.phase[t.NodeID] = PhaseIdle
	}
	return f
}

func (f *fakeFleet) Apply(ctx context.Context, t Target, req ApplyRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, t.NodeID)
	f.sources = append(f.sources, req.Sources)
	if msg := f.refuse[t.NodeID]; msg != "" {
		return fmt.Errorf("%s", msg)
	}
	if f.blackout[t.NodeID] {
		f.offline[t.NodeID] = true // restarts, never comes back
		return nil
	}
	if f.reverts[t.NodeID] {
		f.phase[t.NodeID] = PhaseReverted
		f.lastErr[t.NodeID] = "0 of 4 peers reconnected"
		return nil
	}
	f.version[t.NodeID] = req.Manifest.Version
	f.phase[t.NodeID] = PhaseCommitted
	return nil
}

func (f *fakeFleet) State(ctx context.Context, t Target) (NodeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline[t.NodeID] {
		return NodeState{}, fmt.Errorf("connection refused")
	}
	return NodeState{
		NodeID: t.NodeID, Hostname: t.Hostname,
		Version: f.version[t.NodeID], Phase: f.phase[t.NodeID], LastError: f.lastErr[t.NodeID],
	}, nil
}

func (f *fakeFleet) Connected(nodeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.offline[nodeID]
}

func tenNodes() []Target {
	var out []Target
	for i := 1; i <= 10; i++ {
		out = append(out, Target{
			NodeID:   fmt.Sprintf("node%02d", i),
			Hostname: fmt.Sprintf("peer%02d", i),
			Addr:     netip.AddrFrom4([4]byte{10, 42, 0, byte(i)}),
			Port:     8443,
		})
	}
	return out
}

func testPlan(m Manifest, targets []Target) Plan {
	return Plan{
		Manifest:       m,
		Targets:        targets,
		Canary:         1,
		Batch:          3,
		ConfirmSeconds: 1,
		HealthTimeout:  6 * time.Second,
		Seeds:          []Source{{NodeID: "mgr", Hostname: "manager", Addr: netip.MustParseAddr("10.42.0.99"), Port: 8443}},
	}
}

func TestRolloutHappyPathWavesAndFanout(t *testing.T) {
	m, _, _, _ := signedArtifact(t, "399", false, true)
	targets := tenNodes()
	fleet := newFakeFleet(targets, "398")

	r, err := NewRollout(testPlan(m, targets), fleet, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("rollout: %v", err)
	}
	st := r.Status()
	if st.State != "succeeded" {
		t.Fatalf("state %q: %s", st.State, st.Error)
	}
	for _, n := range st.Nodes {
		if n.State != "ok" || n.Version != "399" {
			t.Fatalf("%s: state=%s version=%s", n.NodeID, n.State, n.Version)
		}
	}
	// Wave 1 is the canary, alone.
	if len(fleet.sources[0]) != 1 || fleet.sources[0][0].NodeID != "mgr" {
		t.Fatalf("the canary should pull from the manager alone, got %v", fleet.sources[0])
	}
	// By the last node, the manager is one source among many — the point of the
	// fanout: ten nodes do not all pull from one uplink.
	last := fleet.sources[len(fleet.sources)-1]
	if len(last) < 5 {
		t.Fatalf("the last wave was only offered %d sources; earlier nodes should have become sources", len(last))
	}
	if st.Nodes[0].Wave != 1 || st.Nodes[1].Wave != 2 {
		t.Fatalf("wave assignment looks wrong: %+v", st.Nodes[:2])
	}
}

// The canary exists for exactly this: the new binary is bad, and nine nodes
// never find out.
func TestRolloutStopsAtTheCanaryAndLeavesTheRestAlone(t *testing.T) {
	m, _, _, _ := signedArtifact(t, "399", false, true)
	targets := tenNodes()
	fleet := newFakeFleet(targets, "398")
	fleet.reverts["node01"] = true // comes back, having backed itself out

	r, _ := NewRollout(testPlan(m, targets), fleet, t.Logf)
	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("a rollout whose canary reverted itself reported success")
	}
	if !strings.Contains(err.Error(), "peers reconnected") {
		t.Fatalf("the failure should carry the node's own reason, got: %v", err)
	}
	st := r.Status()
	if st.State != "failed" {
		t.Fatalf("state %q", st.State)
	}
	if len(fleet.applied) != 1 {
		t.Fatalf("%d nodes were told to apply; only the canary should have been: %v", len(fleet.applied), fleet.applied)
	}
	skipped := 0
	for _, n := range st.Nodes[1:] {
		if n.State != "skipped" {
			t.Fatalf("%s was left in state %q after the canary failed", n.NodeID, n.State)
		}
		skipped++
	}
	if skipped != 9 {
		t.Fatalf("expected 9 untouched nodes, got %d", skipped)
	}
}

// A node that takes the binary and never comes back is the worst case: it is off
// the mesh, so we cannot ask it anything. The rollout must still terminate, and
// must still stop.
func TestRolloutFailsWhenANodeNeverComesBack(t *testing.T) {
	m, _, _, _ := signedArtifact(t, "399", false, true)
	targets := tenNodes()[:4]
	fleet := newFakeFleet(targets, "398")
	fleet.blackout["node01"] = true

	p := testPlan(m, targets)
	p.HealthTimeout = 4 * time.Second
	r, _ := NewRollout(p, fleet, t.Logf)

	start := time.Now()
	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("a node that never came back was counted as upgraded")
	}
	if !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the rollout did not bound its wait")
	}
	if len(fleet.applied) != 1 {
		t.Fatalf("the rollout continued past a node that vanished: %v", fleet.applied)
	}
}

// Upgrading seven of ten nodes leaves a fleet in two versions. That might be
// what the operator wants, but they have to say so.
func TestRolloutRefusesToStartWithNodesOffTheMesh(t *testing.T) {
	m, _, _, _ := signedArtifact(t, "399", false, true)
	targets := tenNodes()
	fleet := newFakeFleet(targets, "398")
	fleet.offline["node03"] = true
	fleet.offline["node07"] = true

	r, _ := NewRollout(testPlan(m, targets), fleet, t.Logf)
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not on the mesh") {
		t.Fatalf("want a refusal naming the offline nodes, got %v", err)
	}
	if len(fleet.applied) != 0 {
		t.Fatal("nodes were upgraded despite the refusal")
	}

	p := testPlan(m, targets)
	p.SkipUnreachable = true
	r2, _ := NewRollout(p, fleet, t.Logf)
	if err := r2.Run(context.Background()); err != nil {
		t.Fatalf("with -skip-unreachable it should proceed: %v", err)
	}
	if len(fleet.applied) != 8 {
		t.Fatalf("upgraded %d nodes, want the 8 that were reachable", len(fleet.applied))
	}
	for _, n := range r2.Status().Nodes {
		if n.NodeID == "node03" && n.State != "skipped" {
			t.Fatalf("node03 was offline but ended in state %q", n.State)
		}
	}
}

func TestRolloutRefusalIsReportedPerNode(t *testing.T) {
	m, _, _, _ := signedArtifact(t, "399", false, true)
	targets := tenNodes()[:3]
	fleet := newFakeFleet(targets, "398")
	fleet.refuse["node01"] = "upgrade: artifact is linux/arm64, this node is linux/amd64"

	r, _ := NewRollout(testPlan(m, targets), fleet, t.Logf)
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("a node that refused the artifact was counted as upgraded")
	}
	n := r.Status().Nodes[0]
	if n.State != "failed" || !strings.Contains(n.Error, "arm64") {
		t.Fatalf("the node's own refusal should be surfaced verbatim: %+v", n)
	}
}
