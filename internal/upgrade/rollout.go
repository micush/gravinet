package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// A rollout is the part an operator actually thinks about: ten nodes, one new
// binary, and the strong preference that if the binary is bad, they find out on
// node one rather than node ten.
//
// The shape is deliberately unclever — a canary wave, then batches, stopping
// dead at the first failure. What makes it work on a mesh is not the scheduling
// but *what counts as success*: a node is not "upgraded" because it accepted the
// command, or because its process is running, but because it came back onto the
// mesh, under the new version, and said so from the far side of a restart. That
// is the only signal that distinguishes a good upgrade from one that will be
// discovered tomorrow.

// Target is one node a rollout can reach.
type Target struct {
	NodeID   string
	Hostname string
	Addr     netip.Addr // overlay address
	Port     int        // web-admin port
}

// ApplyRequest is what a manager asks a peer to do.
type ApplyRequest struct {
	Manifest Manifest `json:"manifest"`
	// Sources are peers already known to hold this artifact, for the node to
	// pull the bytes from (see fetch.go). The manager sends this rather than
	// the artifact itself, and that is the whole distribution design in one
	// field: the control plane carries a few hundred bytes of signed metadata
	// over the mesh's control channel, and the tens of megabytes travel
	// directly between peers over the overlay, from whichever holder is nearest.
	//
	// The list grows as the rollout proceeds — wave two pulls from wave one, not
	// from the manager — so a ten-node fleet does not funnel ten copies of the
	// binary through one node's uplink, and a rollout does not collapse if the
	// manager goes away halfway through. Nothing about a source is trusted: the
	// signature is checked on ingest no matter who served the bytes, so the worst
	// a hostile source can do is waste a node's bandwidth.
	Sources           []Source `json:"sources,omitempty"`
	ConfirmSeconds    int      `json:"confirm_seconds,omitempty"`
	AllowDowngrade    bool     `json:"allow_downgrade,omitempty"`
	AllowPAMDowngrade bool     `json:"allow_pam_downgrade,omitempty"`
}

// NodeState is what a peer reports about itself (the /api/upgrade/state reply).
type NodeState struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`
	// Manageable is false for a peer that is on the mesh but not in Managed
	// mode. Such a peer is not a rollout target and never can be from here; it
	// is reported anyway (see the fleet op) because a node the manager cannot
	// see is precisely the node that gets left on the old binary.
	Manageable bool     `json:"manageable"`
	Version    string   `json:"version"`
	Phase      Phase    `json:"phase"`
	Staged     []string `json:"staged,omitempty"` // artifact ids held in its store
	LastError  string   `json:"last_error,omitempty"`
	Boots      int      `json:"boots,omitempty"`
}

// Fleet is the manager's handle on its peers. Implemented over the existing
// managed-cluster proxy (one HTTPS hop across the overlay to the peer's web
// admin); an interface here so the orchestration can be tested against a fleet
// of ten fake nodes that fail in interesting ways, which is the only way anyone
// is ever going to exercise the "node six crash-loops" path.
type Fleet interface {
	Apply(ctx context.Context, t Target, req ApplyRequest) error
	State(ctx context.Context, t Target) (NodeState, error)
	// Connected reports whether this node currently holds a mesh session with
	// the target. During a peer's restart the answer is no, and that is not an
	// error — it is the expected middle of a successful upgrade.
	Connected(nodeID string) bool
}

// Plan is a rollout's inputs.
type Plan struct {
	Manifest Manifest
	Targets  []Target

	// Canary is how many nodes go in the first wave. Default 1. The canary is
	// the only wave whose size actually matters: it is the difference between
	// finding out the arm64 build went to the amd64 boxes on one node or on
	// four.
	Canary int
	// Batch is the size of every wave after the canary. Default 2. On a
	// ten-node full mesh there is no quorum to preserve, but there is still a
	// reason not to restart everything at once: peers relay for each other, and
	// a node whose only path to the mesh is a relay through a peer that is
	// mid-restart has a much more exciting upgrade than it needed to.
	Batch int
	// HealthTimeout is how long a node has, after being told to apply, to come
	// back on the new version. Must comfortably exceed the node's own confirm
	// window (the guard needs time to *decide* before we conclude it failed) or
	// a rollout will call a node dead while it is still deciding whether it is.
	HealthTimeout time.Duration
	// ConfirmSeconds is the confirm window handed to each node's guard.
	ConfirmSeconds int

	// Seeds are the peers that already hold the artifact when the rollout
	// starts — in practice just the manager itself, which is where the operator
	// loaded it. Every node that completes is added to this set for the waves
	// behind it.
	Seeds []Source

	AllowDowngrade    bool
	AllowPAMDowngrade bool
	// SkipUnreachable proceeds without nodes that are not currently on the mesh
	// rather than refusing to start. Off by default: "three of your ten nodes
	// were offline and I upgraded the other seven" is a fleet in two versions,
	// and the operator should say out loud that they want that.
	SkipUnreachable bool
	// DryRun asks every node to preflight without swapping anything. Answers
	// "would this land?" for the whole fleet at once, which is the question
	// worth asking before the answer costs a restart.
	DryRun bool
}

func (p *Plan) withDefaults() {
	if p.Canary <= 0 {
		p.Canary = 1
	}
	if p.Batch <= 0 {
		p.Batch = 2
	}
	if p.ConfirmSeconds <= 0 {
		p.ConfirmSeconds = int(DefaultConfirmWindow / time.Second)
	}
	if p.HealthTimeout <= 0 {
		// Two confirm windows plus slack: the node has to restart, come up,
		// rejoin, and only *then* does its own guard start counting. Cutting
		// this close means declaring a node failed while it is still succeeding.
		p.HealthTimeout = 2*time.Duration(p.ConfirmSeconds)*time.Second + 60*time.Second
	}
}

// Node/rollout status, snapshotted for the UI and the CLI's progress output.

type NodeStatus struct {
	NodeID   string    `json:"node_id"`
	Hostname string    `json:"hostname"`
	Wave     int       `json:"wave"`
	State    string    `json:"state"` // queued|applying|waiting|ok|failed|skipped
	Version  string    `json:"version,omitempty"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
}

type Status struct {
	Manifest Manifest     `json:"manifest"`
	State    string       `json:"state"` // running|succeeded|failed|aborted
	DryRun   bool         `json:"dry_run,omitempty"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished,omitempty"`
	Error    string       `json:"error,omitempty"`
	Nodes    []NodeStatus `json:"nodes"`
}

// Rollout executes a Plan and exposes progress while it runs.
type Rollout struct {
	plan  Plan
	fleet Fleet
	logf  func(string, ...any)

	mu      sync.Mutex
	st      Status
	holders []Source // peers known to hold the artifact; grows as nodes complete
}

// sources snapshots the current holder set for an ApplyRequest.
func (r *Rollout) sources() []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Source(nil), r.holders...)
}

// addHolder records that a node now has the artifact, making it a source for
// every wave that follows.
func (r *Rollout) addHolder(t Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.holders {
		if h.NodeID == t.NodeID {
			return
		}
	}
	r.holders = append(r.holders, Source{
		NodeID: t.NodeID, Hostname: t.Hostname, Addr: t.Addr, Port: t.Port, Seen: time.Now(),
	})
}

// NewRollout prepares (but does not start) a rollout.
func NewRollout(p Plan, f Fleet, logf func(string, ...any)) (*Rollout, error) {
	p.withDefaults()
	if err := p.Manifest.Validate(); err != nil {
		return nil, err
	}
	if len(p.Targets) == 0 {
		return nil, errors.New("upgrade: rollout has no targets")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Rollout{plan: p, fleet: f, logf: logf, holders: append([]Source(nil), p.Seeds...)}
	r.st = Status{
		Manifest: p.Manifest,
		State:    "running",
		DryRun:   p.DryRun,
		Started:  time.Now(),
		Nodes:    make([]NodeStatus, 0, len(p.Targets)),
	}
	for _, t := range p.Targets {
		r.st.Nodes = append(r.st.Nodes, NodeStatus{NodeID: t.NodeID, Hostname: t.Hostname, State: "queued"})
	}
	return r, nil
}

// Status returns a snapshot safe to serialize while the rollout runs.
func (r *Rollout) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st
	s.Nodes = append([]NodeStatus(nil), r.st.Nodes...)
	return s
}

func (r *Rollout) setNode(nodeID string, f func(*NodeStatus)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.st.Nodes {
		if r.st.Nodes[i].NodeID == nodeID {
			f(&r.st.Nodes[i])
			return
		}
	}
}

func (r *Rollout) finish(state, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.State = state
	r.st.Error = errMsg
	r.st.Finished = time.Now()
}

// Run executes the plan: reachability check, then wave by wave, stopping at the
// first node that does not come back. Blocking; callers that want progress poll
// Status from another goroutine.
func (r *Rollout) Run(ctx context.Context) error {
	targets, err := r.reachable(ctx)
	if err != nil {
		r.finish("failed", err.Error())
		return err
	}
	waves := r.waves(targets)
	for wi, wave := range waves {
		if err := ctx.Err(); err != nil {
			r.markRemaining("skipped", "rollout cancelled")
			r.finish("aborted", err.Error())
			return err
		}
		label := fmt.Sprintf("wave %d/%d (%d node(s))", wi+1, len(waves), len(wave))
		if wi == 0 && r.plan.Canary > 0 {
			label += " [canary]"
		}
		r.logf("upgrade: %s -> %s", label, r.plan.Manifest.Version)

		if err := r.runWave(ctx, wi, wave); err != nil {
			// Stop the whole rollout. The failed node backs itself out on its
			// own schedule (its guard owns that, and it does not need us — which
			// is the entire reason the guard is on the node rather than here:
			// the manager cannot rescue a node that has fallen off the mesh, so
			// it must not be the thing responsible for rescuing it).
			r.markRemaining("skipped", "rollout stopped after an earlier failure")
			r.finish("failed", err.Error())
			r.logf("upgrade: rollout stopped: %v", err)
			return err
		}
	}
	r.finish("succeeded", "")
	r.logf("upgrade: rollout to %s complete across %d node(s)", r.plan.Manifest.Version, len(targets))
	return nil
}

// reachable filters the target list to nodes currently on the mesh, honoring
// SkipUnreachable.
func (r *Rollout) reachable(ctx context.Context) ([]Target, error) {
	var out []Target
	var missing []string
	for _, t := range r.plan.Targets {
		if r.fleet.Connected(t.NodeID) {
			out = append(out, t)
			continue
		}
		missing = append(missing, labelOf(t))
		r.setNode(t.NodeID, func(n *NodeStatus) {
			n.State = "skipped"
			n.Error = "not connected to the mesh"
		})
	}
	if len(missing) > 0 && !r.plan.SkipUnreachable {
		return nil, fmt.Errorf("upgrade: %d target(s) are not on the mesh right now (%v) — "+
			"they would be left on the old version; pass -skip-unreachable to proceed anyway", len(missing), missing)
	}
	if len(out) == 0 {
		return nil, errors.New("upgrade: no reachable targets")
	}
	return out, nil
}

func (r *Rollout) waves(targets []Target) [][]Target {
	var out [][]Target
	i := 0
	if n := r.plan.Canary; n > 0 && n < len(targets) {
		out = append(out, targets[:n])
		i = n
	} else if n >= len(targets) {
		return [][]Target{targets}
	}
	for i < len(targets) {
		j := i + r.plan.Batch
		if j > len(targets) {
			j = len(targets)
		}
		out = append(out, targets[i:j])
		i = j
	}
	return out
}

// runWave applies to every node in the wave in parallel, then waits for all of
// them to come back healthy. Apply and wait are separated on purpose: telling
// four nodes to restart and *then* watching all four is a wave; telling one node
// to restart, watching it, then telling the next, is four waves with extra steps.
func (r *Rollout) runWave(ctx context.Context, wi int, wave []Target) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(wave))
	for _, t := range wave {
		t := t
		r.setNode(t.NodeID, func(n *NodeStatus) {
			n.Wave = wi + 1
			n.State = "applying"
			n.Started = time.Now()
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.applyOne(ctx, t); err != nil {
				r.setNode(t.NodeID, func(n *NodeStatus) {
					n.State = "failed"
					n.Error = err.Error()
					n.Finished = time.Now()
				})
				errCh <- fmt.Errorf("%s: %w", labelOf(t), err)
				return
			}
			r.setNode(t.NodeID, func(n *NodeStatus) {
				n.State = "ok"
				n.Version = r.plan.Manifest.Version
				n.Finished = time.Now()
			})
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}

// applyOne pushes the apply and then waits for the node to reappear on the new
// version.
func (r *Rollout) applyOne(ctx context.Context, t Target) error {
	req := ApplyRequest{
		Manifest:          r.plan.Manifest,
		Sources:           r.sources(),
		ConfirmSeconds:    r.plan.ConfirmSeconds,
		AllowDowngrade:    r.plan.AllowDowngrade,
		AllowPAMDowngrade: r.plan.AllowPAMDowngrade,
	}
	if r.plan.DryRun {
		// The peer's handler treats a dry run as "fetch, verify, preflight, and
		// throw away the result" — every check the real thing does, up to but
		// not including the two renames.
		return r.fleet.Apply(ctx, t, ApplyRequest{Manifest: r.plan.Manifest, Sources: r.sources(), ConfirmSeconds: -1})
	}
	if err := r.fleet.Apply(ctx, t, req); err != nil {
		return err
	}
	// The node has the bytes and has swapped them in; it is now a source for
	// the waves behind it, whether or not it comes back healthy — and if it does
	// not come back, the rollout stops before anyone tries to pull from it.
	r.addHolder(t)
	r.setNode(t.NodeID, func(n *NodeStatus) { n.State = "waiting" })
	return r.waitHealthy(ctx, t)
}

// waitHealthy blocks until the target reports the new version in a committed
// (or at least pending-and-progressing) phase, or the timeout expires.
//
// The subtlety is that *everything* about this node is expected to fail for a
// while: the mesh session drops when it restarts, the proxy hop to it fails,
// its web admin is not listening yet. Treating any of that as a failure would
// make every successful upgrade look like a failed one. So errors are absorbed
// silently until the deadline, and only the *deadline* is fatal — the question
// being asked is not "is anything wrong right now" but "did it come back".
func (r *Rollout) waitHealthy(ctx context.Context, t Target) error {
	deadline := time.Now().Add(r.plan.HealthTimeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			if lastErr == "" {
				lastErr = "no reply from the node before the deadline"
			}
			return fmt.Errorf("did not come back on %s within %s (%s) — "+
				"the node's own guard will revert it to %s if it cannot confirm health",
				r.plan.Manifest.Version, r.plan.HealthTimeout.Round(time.Second), lastErr, "the previous version")
		}
		if !r.fleet.Connected(t.NodeID) {
			lastErr = "still off the mesh (restarting)"
			continue
		}
		st, err := r.fleet.State(ctx, t)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		switch {
		case st.Phase == PhaseReverted:
			// The node made its own decision and backed out. Report its reason,
			// not ours — it knows why and we are guessing.
			why := st.LastError
			if why == "" {
				why = "reverted itself"
			}
			return fmt.Errorf("the node reverted to %s: %s", st.Version, why)
		case st.Version == r.plan.Manifest.Version && st.Phase == PhaseCommitted:
			return nil
		case st.Version == r.plan.Manifest.Version:
			// Running the new binary, still inside its own confirm window.
			lastErr = fmt.Sprintf("on %s, still confirming health", st.Version)
		default:
			lastErr = fmt.Sprintf("still on %s", st.Version)
		}
	}
}

func (r *Rollout) markRemaining(state, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.st.Nodes {
		switch r.st.Nodes[i].State {
		case "queued":
			r.st.Nodes[i].State = state
			r.st.Nodes[i].Error = reason
		}
	}
}

func labelOf(t Target) string {
	if t.Hostname != "" {
		return t.Hostname
	}
	if len(t.NodeID) > 8 {
		return t.NodeID[:8]
	}
	return t.NodeID
}
