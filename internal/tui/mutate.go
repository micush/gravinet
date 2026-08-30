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
// Running the right binary with the right words is not the whole job,
// though: which flags belong on the end of that argv is its own decision,
// and it is per-leaf, not global. A config-editing leaf (network, key,
// seed, nat, qos, route, traffic bgp, naming, settings, and the system
// leaves that call openCfg) parses its arguments with extractOpt's manual
// scanning, which recognizes -config and nothing else — it resolves the
// control socket to reload from cfg.ControlSocket, the value already
// sitting in the file it just loaded, never from a flag. A control-socket
// leaf (ban, unban, fw, upgrade) never calls config.Load at all and
// registers -sock on its own flag.FlagSet, never -config. And a bare-host
// leaf (System > Resolver/Time/Syslog/Users/Power, which call
// internal/service directly) registers neither, each with its own tiny
// flag.FlagSet holding only the fields that one operation needs. Sending
// the wrong flag to the wrong shape is not a warning: on a leaf using a real
// flag.FlagSet it is an immediate "flag provided but not defined" exit, and
// on a leaf using extractOpt's manual scanning it is worse — a silently
// corrupted positional argument count that fails with a generic usage
// message giving no hint the TUI itself is the cause. cliArgs/cliArgsSock/
// cliArgsBare below are three named functions rather than one with a
// parameter precisely so a call site's choice is visible at a glance, and
// every one of the three is verified per leaf by reading its own argument
// parsing before being used, not inferred from what neighboring leaves do.
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

// cliArgs builds the argv for a config-file-editing leaf: everything under
// network/key/seed/nat/qos/route/traffic bgp/naming/settings (and the
// system leaves that call openCfg — snmp, lldp, dhcp mode, config-history).
// These leaves parse arguments with extractOpt/openCfg's manual scanning,
// which recognizes -config and nothing else; they resolve the control
// socket to reload from cfg.ControlSocket, the value already sitting in the
// file they just loaded via -config, never from a command-line flag. Only
// -config is appended here — see cliArgsSock and cliArgsBare for the other
// two shapes, and mutate.go's own package comment for why getting this
// split wrong is not a cosmetic bug: an unrecognized flag on a leaf that
// uses a real flag.FlagSet (as several of these do — cmdTrafficBGPShow and
// friends each register their own bare -config) is an immediate parse
// error, and on a leaf that uses manual extractOpt scanning instead, a
// stray -sock/-config pair that nothing consumes silently corrupts the
// positional argument count instead, which is worse: it fails with a
// generic usage message that gives no hint the TUI itself is the cause.
func (m *model) cliArgs(args ...string) []string {
	out := append([]string{}, args...)
	if m.cfgPath != "" {
		out = append(out, "-config", m.cfgPath)
	}
	return out
}

// cliArgsSock builds the argv for a control-socket-only leaf: ban, unban,
// fw (every subcommand except "exempt", which is config-based and uses
// cliArgs instead), and upgrade. Confirmed per leaf by reading each one's
// own flag.NewFlagSet call — every one of these registers -sock and never
// -config, because none of them ever call config.Load at all; there is
// nothing for -config to mean to them.
func (m *model) cliArgsSock(args ...string) []string {
	out := append([]string{}, args...)
	if m.sockPath != "" {
		out = append(out, "-sock", m.sockPath)
	}
	return out
}

// cliArgsBare returns args completely unchanged, for the leaves that touch
// neither a config file nor the control socket: System > Resolver's
// hostname/dns, Time's timezone/ntp/clock, Syslog's add/del/clear, Users'
// add/passwd/expiry/del, and Power's reboot/shutdown/cancel. Each of these
// operates directly on host OS state (internal/service) and registers its
// own tiny flag.FlagSet with only the fields that operation needs — no
// -config, no -sock — so appending either would be exactly the immediate
// parse error cliArgs's own comment describes. This exists as a named
// function rather than callers just passing args directly so every call
// site reads the same way and the choice is visibly deliberate, not an
// omission.
func (m *model) cliArgsBare(args ...string) []string {
	return args
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
