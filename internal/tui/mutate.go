package tui

// How a mutation actually reaches disk or the running daemon.
//
// There are two paths, and which one a page uses is not a TUI decision — it
// is decided by how gravinet itself already draws the line between a
// config-file change and live daemon state (see nav.go's package comment on
// data.go's split for the read side of the same distinction). Firewall rules
// turned out to be the clearest example while building this: they are not a
// config.Config field the CLI edits and saves — "gravinet fw add" sends a
// live control-socket command, and persistence is a side effect the daemon
// itself performs. Guessing that shape per subsystem and getting it wrong is
// how a save could silently do nothing, or contend with the daemon over the
// same file. So the primary path here does not guess: it runs the actual
// `gravinet` binary as a subprocess with the exact arguments a person would
// type, and lets the CLI's own already-correct routing decide file vs.
// socket, the same way it already does for a human at a shell. There is one
// implementation of "how does this mutation take effect," reached a third
// way, with zero new code making that decision.
//
// The exception is a short, explicit list: a few fields exist as validated
// config.Config setters (the same ones a person auditing this tree would
// find KeySetDistributed, PeerSetEnabled, and the like) that the CLI has
// simply never grown a verb for — they are reachable today only from the web
// admin's edit.go. Shelling out can't cover those; there is no command to
// shell out to. For exactly that list, and no further, this package calls
// the setter directly and runs its own save-and-reload, mirroring
// cmd/gravinet's own commitCfg orchestration (validate, save, tell the
// daemon) rather than the validation inside it — the setter itself still
// owns every rule about what a valid value is. Each such case says in its
// own comment that this is why it's here, so a future CLI verb for the same
// field is a visible invitation to delete the workaround rather than a
// silent duplicate to trip over.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/control"
)

// mutationTimeout bounds a shelled-out command. Every CLI leaf this reaches
// is either a config-file edit (fast) or a single control-socket round trip
// (fast, or a clear "not reachable" error) — nothing here is expected to run
// long, so a generous but finite budget catches a genuinely hung child
// (a stuck flock on the config, a daemon that accepted the connection but
// never answers) rather than leaving the console waiting forever with no way
// out short of killing the process.
const mutationTimeout = 15 * time.Second

// runGravinet executes the running gravinet binary as a subprocess with args,
// exactly as if the operator had typed `gravinet <args...>` at a shell. It is
// a package variable rather than a plain function so tests can substitute a
// fake without a real binary or a real config/daemon on the test host — see
// mutate_test.go.
var runGravinet = func(args ...string) (output string, exitCode int, err error) {
	self, err := os.Executable()
	if err != nil {
		return "", -1, fmt.Errorf("could not locate the running gravinet binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mutationTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, args...)
	// No inherited stdin: every leaf this reaches is fully flag-driven (see
	// this file's package comment on how that was confirmed), so there is
	// nothing for a child to legitimately read from stdin, and connecting it
	// to /dev/null (Cmd's default when Stdin is nil) turns any command that
	// unexpectedly tried to prompt into an instant EOF rather than a console
	// that hangs with no visible cause.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	out := buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		return out, -1, fmt.Errorf("gravinet %s: timed out after %s", strings.Join(args, " "), mutationTimeout)
	}
	if runErr == nil {
		return out, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return out, exitErr.ExitCode(), nil // a clean failure: the CLI itself said no
	}
	return out, -1, fmt.Errorf("could not run gravinet: %w", runErr) // couldn't even start it
}

// mutationResult is what an action reports back, regardless of which path it
// took. ok distinguishes "the operation failed" from "the operation
// succeeded" for styling; detail is shown verbatim, since a CLI leaf's own
// success/error text is already written for a human to read and re-wrapping
// it would just paraphrase it worse.
type mutationResult struct {
	ok     bool
	detail string
}

// runLeaf shells out to a CLI leaf and turns its exit code into a
// mutationResult. group/section name the leaf purely for the case where
// os.Executable fails or the process can't be started at all — a failure
// with no CLI output of its own to show.
func runLeaf(args ...string) mutationResult {
	out, code, err := runGravinet(args...)
	out = strings.TrimSpace(out)
	if err != nil {
		if out != "" {
			return mutationResult{ok: false, detail: out + "\n" + err.Error()}
		}
		return mutationResult{ok: false, detail: err.Error()}
	}
	if code != 0 {
		if out == "" {
			out = fmt.Sprintf("gravinet %s exited %d with no output", strings.Join(args, " "), code)
		}
		return mutationResult{ok: false, detail: out}
	}
	if out == "" {
		out = "ok"
	}
	return mutationResult{ok: true, detail: out}
}

// cliArgs builds the common prefix every shelled-out leaf needs: this
// console's own config and control-socket paths, so the subprocess acts on
// the exact node being viewed rather than whatever this platform's bare
// defaults would resolve to. Appended rather than prepended — extractOpt on
// the CLI side (cmd/gravinet/cli_config.go) scans the whole argument list
// regardless of position, so where these land doesn't matter to it, and
// putting them last keeps every call site below reading as the command a
// person would actually type, with the paths as an inv isible suffix.
func (m *model) cliArgs(args ...string) []string {
	out := append([]string{}, args...)
	if m.cfgPath != "" {
		out = append(out, "-config", m.cfgPath)
	}
	if m.sockPath != "" {
		out = append(out, "-sock", m.sockPath)
	}
	return out
}

// ---- the direct-config fallback -----------------------------------------
//
// See the package comment for exactly when this is the right tool: a
// validated config.Config setter with no CLI verb reaching it yet.

// commitConfig saves cfg to path and, if a daemon is listening on sockPath,
// asks it to reload — the same two steps cmd/gravinet's own commitCfg
// performs, in the same order, for the same reason (validate before writing
// anything, so a rejected value never touches disk). Structural changes that
// commitCfgStructural would instead restart the service for are not run
// through here — see each call site for why the specific field it uses this
// path for doesn't need that.
func commitConfig(cfg *config.Config, path, sockPath string) mutationResult {
	if err := cfg.Validate(); err != nil {
		return mutationResult{ok: false, detail: "invalid after change: " + err.Error()}
	}
	if err := cfg.SaveTo(path); err != nil {
		return mutationResult{ok: false, detail: "save: " + err.Error()}
	}
	endpoint, _ := config.NormalizeControlSocket(sockPath)
	resp, err := control.Do(endpoint, control.Request{Cmd: "reload", Notes: "tui edit"})
	if err == nil && resp.Error == "" {
		return mutationResult{ok: true, detail: "saved and reloaded"}
	}
	return mutationResult{ok: true, detail: "saved — daemon not reachable, applies when it starts"}
}
