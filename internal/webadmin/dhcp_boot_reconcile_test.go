package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
)

// The whole decision table, one case per row.
//
// This is the risky part of reconcileKeaAtBoot and the part that cannot be
// exercised through the function itself: keaConfPath is a constant, so a test
// of the real thing would write to /etc/kea and drive systemd.
func TestKeaBootDecisionTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   keaBootState
		want keaBootAction
		why  string
	}{{
		name: "relay mode",
		st:   keaBootState{Mode: config.DHCPRelay, Servable: 3, Installed: true, Owned: true},
		want: keaBootNothing,
		why:  "Kea is not this node's business in relay mode; the caller has already re-asserted the exclusion",
	}, {
		name: "off",
		st:   keaBootState{Mode: config.DHCPOff, Installed: true, Owned: true},
		want: keaBootNothing,
		why:  "a node that does nothing about DHCP renders no Kea config",
	}, {
		name: "server, file already agrees",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 2, Installed: true, Owned: true, Matches: true},
		want: keaBootNothing,
		why:  "this is every ordinary restart, and it must not write, restart or run systemctl",
	}, {
		name: "server, file is stale",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 2, Installed: true, Owned: true, Matches: false},
		want: keaBootRewrite,
		why:  "the restore case: the stored config changed and nothing re-rendered Kea",
	}, {
		name: "server, no file at all",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 1, Installed: true, Owned: true, Matches: false},
		want: keaBootRewrite,
		why:  "keaOwned reports an absent file as ours, so a restore onto a fresh host writes one",
	}, {
		name: "server, someone else's file",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 2, Installed: true, Owned: false, Matches: false},
		want: keaBootNotOurs,
		why:  "a hand-maintained kea-dhcp4.conf is never taken over during a boot",
	}, {
		name: "server, nothing servable",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 0, Installed: true, Owned: true},
		want: keaBootStopDisable,
		why:  "an enabled unit would come back serving subnets the operator has since removed",
	}, {
		name: "server, nothing servable, Kea not installed",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 0, Installed: false, Owned: true},
		want: keaBootStopDisable,
		why:  "nothing-servable outranks not-installed: there is no config to render either way, and a stale enabled unit still needs disabling",
	}, {
		name: "server, Kea not installed",
		st:   keaBootState{Mode: config.DHCPServer, Servable: 2, Installed: false, Owned: true},
		want: keaBootNotInstalled,
		why:  "boot does not pull a DHCP server down from the distribution; saving the page does",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keaBootDecision(tc.st); got != tc.want {
				t.Errorf("keaBootDecision(%+v) = %v, want %v — %s", tc.st, got, tc.want, tc.why)
			}
		})
	}
}

// The no-churn property, stated on its own because it is the objection this
// whole change had to answer. A node nobody has touched restarts, the file
// matches, and nothing happens — no write, no restart, no systemctl.
//
// If this ever regresses to "re-apply at boot", every gravinet restart bounces
// Kea and every lease renewal in flight goes with it.
func TestKeaBootDoesNothingWhenTheFileAlreadyAgrees(t *testing.T) {
	st := keaBootState{Mode: config.DHCPServer, Servable: 4, Installed: true, Owned: true, Matches: true}
	if got := keaBootDecision(st); got != keaBootNothing {
		t.Fatalf("a node whose Kea config already matches its stored config decided %v, want keaBootNothing", got)
	}
	// And Matches is the only thing standing between that node and a rewrite,
	// so it must be computed from the bytes rather than assumed.
	src := mustRead("dhcp_apply.go")
	fn := between(t, src, "func reconcileKeaAtBoot(", "\n}")
	if !strings.Contains(fn, "bytes.Equal") {
		t.Error("reconcileKeaAtBoot no longer compares the file's bytes; it cannot know whether it agrees")
	}
	if !strings.Contains(fn, "renderKea(served)") {
		t.Error("reconcileKeaAtBoot no longer renders the stored configuration to compare against")
	}
}

// Boot must never install Kea or set aside an operator's config. Both are
// applyDHCP's to do, on an explicit save, and both would be a daemon start
// making a decision nobody asked for.
func TestKeaBootNeverInstallsOrSetsAside(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	fn := between(t, src, "func reconcileKeaAtBoot(", "\n}")
	if containsCall(fn, "installKea") {
		t.Error("reconcileKeaAtBoot installs Kea at daemon startup; that belongs to an explicit save (v951)")
	}
	if containsCall(fn, "setAsideKeaConf") {
		t.Error("reconcileKeaAtBoot moves an operator's kea-dhcp4.conf aside at daemon startup; nothing at boot justifies that")
	}
	// A stop without a disable is the v950 bug, and it looks perfectly
	// reasonable on the line it is written on.
	if strings.Contains(fn, `keaService("stop")`) {
		t.Error("reconcileKeaAtBoot stops Kea without disabling it, so the stop will not survive a reboot")
	}
}

// The reconcile has to actually be wired into the boot path, and has to run
// whatever the relay half does — a node that serves has no relay links, so
// hanging it off the relay's own early return would mean it never ran on
// exactly the nodes it is for.
func TestStartDHCPRelayReconcilesKea(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	boot := between(t, src, "func StartDHCPRelay(", "\n}")
	if !strings.Contains(boot, "reconcileKeaAtBoot(c)") {
		t.Fatal("daemon startup does not reconcile Kea against the stored configuration; a restored server config reaches Kea only when somebody re-saves the page")
	}
	iRec := strings.Index(boot, "reconcileKeaAtBoot(c)")
	iRet := strings.Index(boot, "if !c.RelayActive()")
	if iRet >= 0 && iRec > iRet {
		t.Error("the reconcile is behind the relay's early return, so it never runs on a server-mode node — which is every node it exists for")
	}
}

// Its failures must not surface as relay failures: the caller logs what it
// gets back under "dhcp relay:", which would send a reader to the wrong half
// of the DHCP page.
func TestKeaBootReportsItsOwnFailures(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	if !strings.Contains(src, "func reconcileKeaAtBoot(c config.DHCPConfig) {") {
		t.Fatal("reconcileKeaAtBoot returns something; its failures belong in the log, not in the relay's error channel")
	}
	fn := between(t, src, "func reconcileKeaAtBoot(", "\n}")
	if !strings.Contains(fn, "logx.Warnf") {
		t.Error("reconcileKeaAtBoot reports no failures at all")
	}
}
