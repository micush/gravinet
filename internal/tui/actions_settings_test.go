package tui

// Argv checks for every editable Settings row, the same discipline
// actions_mesh_test.go applies to Mesh: each test opens the row's real form
// through mnemonicAction (not by calling a submit func directly), fills it
// in, submits, and asserts on exactly what runGravinet received.

import (
	"testing"
)

// openMnemonicForm finds the row on the current page whose label is exactly
// label and opens its form via the real mnemonic path, failing the test if
// no such editable row exists — a stronger check than calling row.edit
// directly, since it also proves the row was actually wired up with an edit
// func and picked up a mnemonic.
func openMnemonicForm(t *testing.T, m *model, label string) {
	t.Helper()
	cards := m.currentCards()
	assignMnemonicsInPlace(cards)
	for _, cd := range cards {
		for _, it := range cd.items {
			ekv, ok := it.(editableKV)
			if !ok {
				continue
			}
			for _, row := range ekv.rows {
				if row.k == label {
					if row.edit == nil {
						t.Fatalf("row %q has no edit function", label)
					}
					if row.mnemonic == 0 {
						t.Fatalf("row %q has an edit function but no mnemonic was assigned", label)
					}
					m.openForm(row.edit(m))
					return
				}
			}
		}
	}
	t.Fatalf("no editable row labeled %q found on %q", label, m.section)
}

func settingsTestModel(t *testing.T) (*model, *fakeGravinet) {
	t.Helper()
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("settings")
	return m, f
}

func TestSettingsWebAdminListenArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "listen")
	submitCurrentForm(t, m, map[string]string{"v": "127.0.0.1,10.0.0.5"})
	hasArgs(t, lastCall(f), "settings", "listen-addrs", "127.0.0.1,10.0.0.5")
}

func TestSettingsLoginLockoutArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "login lockout")
	submitCurrentForm(t, m, map[string]string{"attempts": "5", "seconds": "600"})
	hasArgs(t, lastCall(f), "settings", "login-ban", "5", "600")
}

func TestSettingsHistoryLimitArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "config history limit")
	submitCurrentForm(t, m, map[string]string{"n": "50"})
	hasArgs(t, lastCall(f), "settings", "history-limit", "50")
}

func TestSettingsRemoteShellArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "remote shell")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "shell", "on")
}

func TestSettingsGeoIPArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "geoip lookup")
	submitCurrentForm(t, m, map[string]string{"on": "false"})
	hasArgs(t, lastCall(f), "settings", "geoip", "off")
}

func TestSettingsManagedArgv(t *testing.T) {
	// Managed/manager are top-level commands, not "settings X" — the one
	// place this group's forms deliberately don't share the generic prefix.
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "managed")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	got := lastCall(f)
	if len(got) == 0 || got[0] != "managed" {
		t.Fatalf("expected the first arg to be \"managed\", got %v", got)
	}
	hasArgs(t, got, "managed", "on")
}

func TestSettingsManagerArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "manager")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	got := lastCall(f)
	if len(got) == 0 || got[0] != "manager" {
		t.Fatalf("expected the first arg to be \"manager\", got %v", got)
	}
}

func TestSettingsAcceptManagerUpgradesArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "accept manager upgrades")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "accept-mgr-upgrades", "on")
}

func TestSettingsLogLevelArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "log level")
	submitCurrentForm(t, m, map[string]string{"v": "debug"})
	hasArgs(t, lastCall(f), "settings", "log-level", "debug")
}

func TestSettingsLogSizeArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "log size cap")
	submitCurrentForm(t, m, map[string]string{"v": "500M"})
	hasArgs(t, lastCall(f), "settings", "log-size", "500M")
}

func TestSettingsUDPPortsArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "udp ports")
	submitCurrentForm(t, m, map[string]string{"v": "51820,51821"})
	hasArgs(t, lastCall(f), "settings", "udp-port", "51820,51821")
}

func TestSettingsUDPPortsOffArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "udp ports")
	submitCurrentForm(t, m, map[string]string{"v": "-"})
	hasArgs(t, lastCall(f), "settings", "udp-port", "-")
}

func TestSettingsTCPPortsArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "tcp ports")
	submitCurrentForm(t, m, map[string]string{"v": "443"})
	hasArgs(t, lastCall(f), "settings", "tcp-port", "443")
}

func TestSettingsKeepaliveArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "keepalive")
	submitCurrentForm(t, m, map[string]string{"n": "20"})
	hasArgs(t, lastCall(f), "settings", "keepalive", "20")
}

func TestSettingsPeerTimeoutArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "peer timeout")
	submitCurrentForm(t, m, map[string]string{"n": "90"})
	hasArgs(t, lastCall(f), "settings", "peer-timeout", "90")
}

func TestSettingsRouteAdvArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "route re-advertise")
	submitCurrentForm(t, m, map[string]string{"n": "30"})
	hasArgs(t, lastCall(f), "settings", "route-adv", "30")
}

func TestSettingsNATStateArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "nat state timeout")
	submitCurrentForm(t, m, map[string]string{"n": "60"})
	hasArgs(t, lastCall(f), "settings", "nat-state", "60")
}

func TestSettingsUPnPArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "upnp")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "upnp", "on")
}

func TestSettingsIPForwardingArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "ip forwarding")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "ip-forwarding", "on")
}

func TestSettingsIPRedirectsArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "icmp redirects suppressed")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "ip-redirects", "on")
}

func TestSettingsWorkerThreadsArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "worker threads")
	submitCurrentForm(t, m, map[string]string{"n": "4"})
	hasArgs(t, lastCall(f), "settings", "worker-threads", "4")
}

func TestSettingsTunQueuesArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "tun queues")
	submitCurrentForm(t, m, map[string]string{"n": "2"})
	hasArgs(t, lastCall(f), "settings", "tun-queues", "2")
}

func TestSettingsSocketBufferArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "socket buffer")
	submitCurrentForm(t, m, map[string]string{"n": "8"})
	hasArgs(t, lastCall(f), "settings", "socket-buffer", "8")
}

func TestSettingsUDPGSOArgv(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "udp gso")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	hasArgs(t, lastCall(f), "settings", "udp-gso", "on")
}

func TestSettingsDDNSFormOnlyCallsChangedFields(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "dynamic dns")
	for i := range m.form.field {
		if m.form.field[i].key == "interval" {
			m.form.field[i].value = "10"
		}
	}
	m.handleKey(key{t: keyEnter})
	if len(f.calls) != 1 {
		t.Fatalf("changing one field should issue exactly one call, got %d: %v", len(f.calls), f.calls)
	}
	hasArgs(t, f.calls[0], "settings", "ddns", "interval", "10")
}

func TestSettingsDDNSKeyArgvClearsWithDash(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "tsig key")
	submitCurrentForm(t, m, map[string]string{"v": ""})
	hasArgs(t, lastCall(f), "settings", "ddns", "key", "-")
}

func TestSettingsDDNSKeyArgvSetsAValue(t *testing.T) {
	m, f := settingsTestModel(t)
	openMnemonicForm(t, m, "tsig key")
	submitCurrentForm(t, m, map[string]string{"v": "example:c2VjcmV0:hmac-sha256"})
	hasArgs(t, lastCall(f), "settings", "ddns", "key", "example:c2VjcmV0:hmac-sha256")
}

// TestSettingsTSIGKeyIsNeverPreFilled makes sure the secret-handling promise
// in ddnsKeyForm's own comment actually holds: opening the form must never
// show whatever key is already configured.
func TestSettingsTSIGKeyIsNeverPreFilled(t *testing.T) {
	m, _ := settingsTestModel(t)
	m.snap.cfg.DDNS.TSIGKey = "example:c2VjcmV0dGhpbmc=:hmac-sha256"
	openMnemonicForm(t, m, "tsig key")
	for _, fl := range m.form.field {
		if fl.value != "" {
			t.Errorf("field %q was pre-filled with %q — a secret must never be shown", fl.key, fl.value)
		}
	}
}

// TestEveryVisibleSettingsRowHasAWorkingMnemonic walks every editable row on
// the page and confirms it got a real, distinct mnemonic — the general
// property behind all the specific argv checks above.
func TestEveryVisibleSettingsRowHasAWorkingMnemonic(t *testing.T) {
	m, _ := settingsTestModel(t)
	cards := m.currentCards()
	assignMnemonicsInPlace(cards)
	seen := map[rune]string{}
	count := 0
	for _, cd := range cards {
		for _, it := range cd.items {
			ekv, ok := it.(editableKV)
			if !ok {
				continue
			}
			for _, row := range ekv.rows {
				if row.edit == nil {
					continue
				}
				count++
				if row.mnemonic == 0 {
					t.Errorf("row %q has no mnemonic", row.k)
					continue
				}
				if prev, dup := seen[row.mnemonic]; dup {
					t.Errorf("mnemonic %q used by both %q and %q", row.mnemonic, prev, row.k)
				}
				seen[row.mnemonic] = row.k
			}
		}
	}
	if count < 20 {
		t.Errorf("expected at least 20 editable rows on Settings, found %d", count)
	}
}
