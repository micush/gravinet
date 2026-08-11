package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gravinet/internal/upgrade"
)

// A trial record can outlive the process that was watching it: Watch and
// OnBoot are both gated on the upgrade feature being enabled, so a node whose
// trial was interrupted — the feature switched off, the binary replaced out of
// band by the installer, a restart that never ran the guard — keeps a pending
// record nothing will ever advance. Every later apply is then refused by a
// trial that ended weeks ago.
//
// clear forgets that record. It is deliberately not rollback: rollback puts
// the *previous* binary back, which on a stale record means downgrading a node
// that has since moved on several releases — the opposite of what an operator
// in this position wants.
func TestUpgradeClearForgetsStaleTrial(t *testing.T) {
	dir := t.TempDir()
	g := upgrade.NewGuard(dir, nil, func(string, ...any) {})
	u := &upgradeSvc{stateDir: dir, guard: g, version: "847", target: dir + "/gravinet"}

	// A real (if useless) archive path, so apply gets past opening the file
	// and reaches the guard check rather than failing earlier on the open.
	arch := dir + "/candidate.tgz"
	if err := os.WriteFile(arch, []byte("not really an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyBody := []byte(`{"src_path":"` + arch + `"}`)

	// Nothing pending: clear is a no-op that says so, rather than an error.
	out, err := u.controlOp("clear", nil)
	if err != nil {
		t.Fatal(err)
	}
	var idle map[string]any
	if err := json.Unmarshal(out, &idle); err != nil {
		t.Fatal(err)
	}
	if idle["cleared"] != false {
		t.Errorf("clearing an idle guard should report nothing cleared: %v", idle)
	}

	// Put a trial in flight, the way an apply would.
	if err := g.Arm(upgrade.State{From: "823", To: "828", Target: u.target, ConfirmSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	if g.Load().Phase != upgrade.PhasePending {
		t.Fatal("fixture did not enter the pending phase")
	}

	// While pending, a second apply is refused — and the message has to name
	// the way out, or an operator is told to wait for something that will
	// never happen.
	_, applyErr := u.controlOp("apply", applyBody)
	if applyErr == nil {
		t.Fatal("a second apply should be refused while a trial is pending")
	}
	if !strings.Contains(applyErr.Error(), "gravinet upgrade clear") {
		t.Errorf("the refusal should point at the escape hatch, got: %v", applyErr)
	}
	if !strings.Contains(applyErr.Error(), "does not change the running binary") {
		t.Errorf("the refusal should say clear is not a rollback, got: %v", applyErr)
	}

	// Clear forgets it, naming what it forgot.
	out, err = u.controlOp("clear", nil)
	if err != nil {
		t.Fatal(err)
	}
	var cleared map[string]any
	if err := json.Unmarshal(out, &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared["cleared"] != true || cleared["from"] != "823" || cleared["to"] != "828" {
		t.Errorf("clear should report what it forgot: %v", cleared)
	}
	if p := g.Load().Phase; p != upgrade.PhaseIdle {
		t.Fatalf("phase after clear = %q, want idle", p)
	}

	// And an apply is no longer blocked by the trial. It still fails — the
	// file is not a real archive — but on its own merits, not the guard's.
	if _, err := u.controlOp("apply", applyBody); err == nil {
		t.Fatal("expected the bogus archive to fail")
	} else if strings.Contains(err.Error(), "mid-trial") {
		t.Errorf("the stale trial is still blocking applies: %v", err)
	}
}
