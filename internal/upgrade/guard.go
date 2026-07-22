package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile lives in the upgrade state directory and records an in-flight
// upgrade across the process boundary the upgrade itself creates. Everything
// in this
// file exists for one reason: a node that cannot rejoin the mesh cannot be
// rescued *over* the mesh, so it has to be able to rescue itself, with no help,
// while in whatever state the new binary left it.
const stateFile = "state.json"

// MaxBoots is how many times the new binary may start without confirming
// health before the guard gives up on it. Three, not one: a single failed start
// can be a transient (a device not yet present at boot, a DHCP lease not yet
// held, an underlay interface still coming up), and reverting the fleet because
// one node lost a race with udev would be its own kind of outage. Three
// consecutive failures is no longer a race.
const MaxBoots = 3

// DefaultConfirmWindow is how long the new binary has, per boot, to reach a
// healthy state before the guard reverts it. It has to comfortably exceed a
// cold start: interfaces up, seeds dialed, at least one handshake completed.
const DefaultConfirmWindow = 90 * time.Second

// Phase is where an upgrade currently stands on this node.
type Phase string

const (
	PhaseIdle      Phase = ""          // nothing in flight
	PhasePending   Phase = "pending"   // binary swapped; the new one has not yet proven itself
	PhaseCommitted Phase = "committed" // the new binary came up and reached health
	PhaseReverted  Phase = "reverted"  // the new binary failed; the old one was restored
)

// State is the guard's persisted record. It is written before the swap and read
// on the very next boot — possibly by a different binary than the one that wrote
// it, which is why it is plain JSON with no clever encoding and no dependency on
// any type outside this file. A state file the new binary cannot parse is a
// state file that cannot rescue the node from the new binary.
type State struct {
	Phase  Phase  `json:"phase"`
	From   string `json:"from_version,omitempty"`
	To     string `json:"to_version,omitempty"`
	Target string `json:"target,omitempty"` // binary path that was swapped

	// Boots counts starts of the new binary since the swap. Incremented at the
	// top of the daemon's run path, before anything that could plausibly fail,
	// so that a binary which crashes during startup still gets counted — the
	// count is the only evidence a crash loop leaves behind.
	Boots int `json:"boots"`

	// PrePeers is how many peers were connected immediately before the swap.
	// This is what makes "healthy" mean something on a mesh: a node that had
	// six peers and now has none has not started successfully, no matter how
	// cleanly its process is running. A node that had none before (a fresh
	// install, a lab box) is not held to that standard, because it would revert
	// every upgrade forever.
	PrePeers int `json:"pre_peers"`

	StartedUnix    int64  `json:"started_unix"`
	ConfirmSeconds int    `json:"confirm_seconds"`
	LastError      string `json:"last_error,omitempty"`
	DecidedUnix    int64  `json:"decided_unix,omitempty"` // when it committed or reverted
}

// Guard owns the state file and the post-swap watchdog.
type Guard struct {
	dir string

	mu sync.Mutex
	// restart is how the guard puts a restored binary back into service after a
	// revert. Injected rather than imported so this package does not depend on
	// internal/service (and so tests can revert without restarting anything).
	restart func() error
	logf    func(string, ...any)
}

// NewGuard binds a guard to the upgrade state directory. restart may be nil, in which
// case a revert restores the binary and reports that a manual restart is needed
// — better than pretending it recovered.
func NewGuard(dir string, restart func() error, logf func(string, ...any)) *Guard {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Guard{dir: dir, restart: restart, logf: logf}
}

func (g *Guard) path() string { return filepath.Join(g.dir, stateFile) }

// Load reads the current state. A missing or unparseable file is Idle, not an
// error: this is called on the startup path of every daemon on every boot,
// including the overwhelming majority that have never upgraded anything, and it
// must never be a reason not to start.
func (g *Guard) Load() State {
	b, err := os.ReadFile(g.path())
	if err != nil {
		return State{Phase: PhaseIdle}
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{Phase: PhaseIdle}
	}
	return s
}

// save writes the state atomically. A half-written state file read by the next
// boot is a node that has forgotten it is mid-upgrade — the one thing this
// whole mechanism must never do.
func (g *Guard) save(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.path() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, g.path())
}

// Arm records a pending upgrade. Called *before* the swap and the restart, so
// that a crash at any point afterwards — including one during the swap itself —
// leaves evidence the next boot can act on. Arming after the swap would leave a
// window in which the node is running new code with no record that it should be
// watching it.
func (g *Guard) Arm(s State) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s.Phase = PhasePending
	s.Boots = 0
	s.StartedUnix = time.Now().Unix()
	if s.ConfirmSeconds <= 0 {
		s.ConfirmSeconds = int(DefaultConfirmWindow / time.Second)
	}
	return g.save(s)
}

// BootAction is what OnBoot decided.
type BootAction int

const (
	BootNormal   BootAction = iota // nothing in flight, or the upgrade is still within its allowance
	BootReverted                   // the new binary was backed out; the restored one needs to run
)

// OnBoot must be called early in the daemon's startup, before anything that can
// fail. It counts this start against the pending upgrade's boot allowance and,
// once the allowance is spent, reverts.
//
// The ordering is the whole point. This runs before the config is applied,
// before interfaces come up, before a single socket is bound — so a new binary
// that dies in *any* of those places still gets counted on its way past here,
// and the third such death restores the binary that was working. The residual
// case this cannot catch is a binary that fails before Go's runtime reaches
// main() at all (a missing shared library, a corrupt ELF header, the wrong
// architecture); that is precisely the case Preflight's ProbeBinary refuses to
// let through the swap in the first place, which is why these two mechanisms
// are worth having *both* of.
func (g *Guard) OnBoot() (BootAction, State) {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.Load()
	if s.Phase != PhasePending {
		return BootNormal, s
	}
	s.Boots++
	if s.Boots <= MaxBoots {
		g.logf("upgrade: boot %d of %d under the new binary (%s -> %s); watching for health",
			s.Boots, MaxBoots, s.From, s.To)
		_ = g.save(s)
		return BootNormal, s
	}
	// Allowance spent. Whatever is wrong with this binary, it is not a transient.
	g.logf("upgrade: the new binary (%s) has failed to confirm health across %d boots — reverting to %s",
		s.To, MaxBoots, s.From)
	s.LastError = fmt.Sprintf("failed to confirm health across %d boots", MaxBoots)
	g.revertLocked(&s)
	return BootReverted, s
}

// Watch arms the post-boot health watchdog: the new binary has ConfirmSeconds to
// satisfy healthy(), or it is backed out.
//
// healthy is supplied by the daemon (it knows what a working node looks like:
// interfaces up, and — if this node had peers before the swap — peers again).
// It is polled rather than awaited so that a node which comes up, joins, and
// then loses the mesh thirty seconds later still fails the window: the question
// is not "did it ever work" but "is it working when the window closes".
//
// Watch returns immediately; it runs until the window closes or ctx-like
// cancellation via stop(). The returned stop function is idempotent.
func (g *Guard) Watch(healthy func() (bool, string)) (stop func()) {
	s := g.Load()
	if s.Phase != PhasePending {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }

	window := time.Duration(s.ConfirmSeconds) * time.Second
	if window <= 0 {
		window = DefaultConfirmWindow
	}
	go func() {
		deadline := time.NewTimer(window)
		defer deadline.Stop()
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		lastReason := "no health report yet"
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				// Poll but do not commit early: see the doc comment. We only
				// record the most recent reason, for the failure message.
				if ok, reason := healthy(); ok {
					lastReason = ""
				} else {
					lastReason = reason
				}
			case <-deadline.C:
				ok, reason := healthy()
				if ok {
					g.Commit()
					return
				}
				if reason == "" {
					reason = lastReason
				}
				g.Fail(reason)
				return
			}
		}
	}()
	return stop
}

// Commit marks the pending upgrade good. The backup binary is deliberately left
// on disk: `gravinet upgrade rollback` is a thing an operator wants at 3am
// *after* an upgrade has been declared healthy, when the regression turns out to
// be the kind health checks do not see.
func (g *Guard) Commit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.Load()
	if s.Phase != PhasePending {
		return
	}
	s.Phase = PhaseCommitted
	s.DecidedUnix = time.Now().Unix()
	s.LastError = ""
	g.logf("upgrade: %s confirmed healthy (was %s) — committed", s.To, s.From)
	_ = g.save(s)
}

// Fail backs the upgrade out and restarts into the restored binary.
func (g *Guard) Fail(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.Load()
	if s.Phase != PhasePending {
		return
	}
	g.logf("upgrade: %s did not reach a healthy state (%s) — reverting to %s", s.To, reason, s.From)
	s.LastError = reason
	g.revertLocked(&s)
}

// revertLocked restores the previous binary, records the outcome, and puts it
// back into service. Callers hold g.mu.
//
// The state file is written *before* the restart is requested, and that order is
// not negotiable: the restart terminates this process, and if the record still
// said "pending" when the restored binary came up, that binary would count
// itself against the failed upgrade's boot allowance and eventually try to
// revert *itself*, having no idea it is already the rescue.
func (g *Guard) revertLocked(s *State) {
	s.Phase = PhaseReverted
	s.DecidedUnix = time.Now().Unix()
	if err := Revert(s.Target); err != nil {
		// There is no third option here. Record it, shout about it, and leave
		// the node for a human — but leave the record accurate, so whoever
		// arrives can see what happened rather than inferring it from a version
		// string that does not match anything.
		s.LastError = "revert failed: " + err.Error()
		g.logf("upgrade: REVERT FAILED: %v — this node needs manual recovery", err)
		_ = g.save(*s)
		return
	}
	_ = g.save(*s)
	if g.restart == nil {
		g.logf("upgrade: previous binary restored at %s; restart the service to run it", s.Target)
		return
	}
	if err := g.restart(); err != nil {
		g.logf("upgrade: previous binary restored, but the automatic restart failed (%v) — "+
			"the node is running new code from an already-replaced binary until it is restarted", err)
	}
}

// Rollback is the operator-driven equivalent of Fail: undo a committed upgrade.
// Unlike the automatic path it does not require a pending state, because the
// whole point is to back out something already declared good.
func (g *Guard) Rollback() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.Load()
	if s.Target == "" {
		return fmt.Errorf("upgrade: no record of an applied upgrade to roll back")
	}
	if err := Revert(s.Target); err != nil {
		return err
	}
	s.Phase = PhaseReverted
	s.LastError = "rolled back by operator"
	s.DecidedUnix = time.Now().Unix()
	if err := g.save(s); err != nil {
		return err
	}
	if g.restart != nil {
		return g.restart()
	}
	return nil
}

// Clear resets to idle. Used after a successful rollback has been observed, and
// by tests.
func (g *Guard) Clear() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.save(State{Phase: PhaseIdle})
}
