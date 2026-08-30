package tui

// Argv checks for System > Resolver, Time, DHCP, and SNMP — the same
// discipline as every other group's tests, with extra attention to the
// cliArgs/cliArgsSock/cliArgsBare split: Resolver and Time are bare-host
// leaves (no -config, no -sock), DHCP and SNMP are config-editing leaves
// (-config only), confirmed against cmd/gravinet's own source before any of
// this was written — see mutate.go's package comment.

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/service"
)

func systemTestModel(t *testing.T, section string) (*model, *fakeGravinet) {
	t.Helper()
	f := installFakeGravinet(t)
	m := newModel(testSnapshot(), "dark", colorMono)
	m.w, m.h = 120, 40
	m.setSection(section)
	return m, f
}

// ---- resolver -----------------------------------------------------------

func TestResolverHostnameArgvIsBare(t *testing.T) {
	m, f := systemTestModel(t, "resolver")
	m.openForm(resolverHostnameForm("old-name"))
	submitCurrentForm(t, m, map[string]string{"v": "new-name"})
	got := lastCall(f)
	hasArgs(t, got, "system", "resolver", "hostname", "new-name")
	hasNotArgs(t, got, "-config", "-sock")
}

// TestResolverPageRendersTheHostnameMnemonic confirms the page itself (not
// just the form in isolation) wires the row up correctly, once the lazy
// fetch has resolved — seeded directly here rather than waiting on a real
// service.HostResolver() call, the same way other lazy-page tests avoid
// depending on the state of the machine running them.
func TestResolverPageRendersTheHostnameMnemonic(t *testing.T) {
	m, _ := systemTestModel(t, "resolver")
	m.lazy.set("resolver", service.ResolverInfo{Hostname: "gn1"}, nil)
	c := m.currentCards()
	rows := c[0].items[0].(editableKV).rows
	if rows[0].k != "hostname" || rows[0].edit == nil {
		t.Fatalf("expected an editable hostname row first, got %+v", rows[0])
	}
}

func TestResolverDNSArgvIsBareAndSplitsServers(t *testing.T) {
	m, f := systemTestModel(t, "resolver")
	m.openForm(resolverDNSForm(service.ResolverInfo{}))
	submitCurrentForm(t, m, map[string]string{"servers": "10.0.0.1, 10.0.0.2", "search": "example.com"})
	got := lastCall(f)
	hasArgs(t, got, "system", "resolver", "dns", "10.0.0.1", "10.0.0.2", "-search", "example.com")
	hasNotArgs(t, got, "-config", "-sock")
}

// ---- time -----------------------------------------------------------

func TestTimeTimezoneArgvIsBare(t *testing.T) {
	m, f := systemTestModel(t, "time")
	m.openForm(timeTimezoneForm(service.TimeInfo{}))
	submitCurrentForm(t, m, map[string]string{"v": "America/Phoenix"})
	got := lastCall(f)
	hasArgs(t, got, "system", "time", "timezone", "America/Phoenix")
	hasNotArgs(t, got, "-config", "-sock")
}

func TestTimeNTPOffArgv(t *testing.T) {
	m, f := systemTestModel(t, "time")
	m.openForm(timeNTPForm(service.TimeInfo{NTPEnabled: true}))
	submitCurrentForm(t, m, map[string]string{"on": "false"})
	hasArgs(t, lastCall(f), "system", "time", "ntp", "off")
}

func TestTimeNTPOnArgvIncludesServers(t *testing.T) {
	m, f := systemTestModel(t, "time")
	m.openForm(timeNTPForm(service.TimeInfo{}))
	submitCurrentForm(t, m, map[string]string{"on": "true", "servers": "pool.ntp.org"})
	hasArgs(t, lastCall(f), "system", "time", "ntp", "on", "pool.ntp.org")
}

func TestTimeClockArgv(t *testing.T) {
	m, f := systemTestModel(t, "time")
	m.openForm(timeClockForm())
	submitCurrentForm(t, m, map[string]string{"v": "2026-08-02T15:04:05"})
	hasArgs(t, lastCall(f), "system", "time", "clock", "2026-08-02T15:04:05")
}

// ---- dhcp -----------------------------------------------------------

func TestDHCPModeArgvIsConfigOnly(t *testing.T) {
	m, f := systemTestModel(t, "dhcp")
	m.openForm(dhcpModeForm(m))
	submitCurrentForm(t, m, map[string]string{"v": "relay"})
	got := lastCall(f)
	hasArgs(t, got, "system", "dhcp", "mode", "relay", "-config")
	hasNotArgs(t, got, "-sock")
}

// ---- snmp -----------------------------------------------------------

func TestSNMPStateArgv(t *testing.T) {
	m, f := systemTestModel(t, "snmp")
	openMnemonicForm(t, m, "state")
	submitCurrentForm(t, m, map[string]string{"on": "true"})
	got := lastCall(f)
	hasArgs(t, got, "system", "snmp", "on", "-config")
	hasNotArgs(t, got, "-sock")
}

func TestSNMPListenArgv(t *testing.T) {
	m, f := systemTestModel(t, "snmp")
	openMnemonicForm(t, m, "listen")
	submitCurrentForm(t, m, map[string]string{"v": "0.0.0.0:161"})
	hasArgs(t, lastCall(f), "system", "snmp", "listen", "0.0.0.0:161")
}

func TestSNMPCommunityNeverDisplaysTheSecret(t *testing.T) {
	m, _ := systemTestModel(t, "snmp")
	m.snap.cfg.SNMP.Communities = []config.SNMPCommunity{{Community: "s3cr3t-do-not-show"}}
	out := drawText(m, 120, 40)
	if containsSubstr(out, "s3cr3t-do-not-show") {
		t.Fatal("the snmp page displayed a community string")
	}
}

func containsSubstr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ---- lldp interfaces ------------------------------------------------------

func TestLLDPAddInterfaceArgvIsConfigOnly(t *testing.T) {
	m, f := systemTestModel(t, "lldp")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"iface": "eth3"})
	got := lastCall(f)
	hasArgs(t, got, "system", "lldp", "iface", "add", "eth3", "-config")
	hasNotArgs(t, got, "-sock")
}

func TestLLDPDeleteInterfaceArgv(t *testing.T) {
	m, f := systemTestModel(t, "lldp")
	m.snap.cfg.Discovery.Interfaces = []config.DiscoveryIface{{Name: "eth0", LLDP: true, CDP: true}}
	m.syncSelection()
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "system", "lldp", "iface", "del", "eth0")
}

// ---- syslog -----------------------------------------------------------

func TestSyslogAddArgvIsBare(t *testing.T) {
	m, f := systemTestModel(t, "syslog")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"host": "logs.example", "proto": "tcp", "port": "601"})
	got := lastCall(f)
	hasArgs(t, got, "system", "syslog", "add", "logs.example", "-proto", "tcp", "-port", "601")
	hasNotArgs(t, got, "-config", "-sock")
}

func TestSyslogDeleteArgv(t *testing.T) {
	m, f := systemTestModel(t, "syslog")
	m.lazy.set("syslog", service.SyslogInfo{Targets: []service.SyslogTarget{{Remote: "logs.example", Port: 514, Protocol: "udp"}}}, nil)
	m.syncSelection()
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	got := lastCall(f)
	hasArgs(t, got, "system", "syslog", "del", "logs.example")
	hasNotArgs(t, got, "-config", "-sock")
}

// ---- users --------------------------------------------------------------

func TestUsersAddRejectsBlankFieldsWithoutShellingOut(t *testing.T) {
	m, f := systemTestModel(t, "users")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"name": "", "pass": ""})
	if len(f.calls) != 0 {
		t.Errorf("a blank name/password should be rejected before shelling out, got %v", f.calls)
	}
}

func TestUsersAddArgvIsBareAndRequiresAPassword(t *testing.T) {
	m, f := systemTestModel(t, "users")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"name": "alice", "pass": "hunter2", "expires": "2027-01-01"})
	got := lastCall(f)
	hasArgs(t, got, "system", "users", "add", "alice", "-pass", "hunter2", "-expires", "2027-01-01")
	hasNotArgs(t, got, "-config", "-sock")
}

func TestUsersExpiryArgv(t *testing.T) {
	m, f := systemTestModel(t, "users")
	m.lazy.set("sys-users", service.UsersInfo{Users: []service.SysUser{{Name: "alice", Exists: true}}}, nil)
	m.syncSelection()
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"date": "2027-06-01"})
	hasArgs(t, lastCall(f), "system", "users", "expiry", "alice", "2027-06-01")
}

func TestUsersDeleteArgv(t *testing.T) {
	m, f := systemTestModel(t, "users")
	m.lazy.set("sys-users", service.UsersInfo{Users: []service.SysUser{{Name: "alice", Exists: true}}}, nil)
	m.syncSelection()
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "system", "users", "del", "alice")
}

// ---- config history -----------------------------------------------------

func TestConfigHistorySnapshotArgv(t *testing.T) {
	m, f := systemTestModel(t, "config-history")
	m.lazy.set("config-history", []config.SnapshotMeta{}, nil)
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"confirm": "true"})
	hasArgs(t, lastCall(f), "system", "config-history", "snapshot", "-config")
}

func TestConfigHistoryDiffArgvRunsImmediatelyNoConfirm(t *testing.T) {
	m, f := systemTestModel(t, "config-history")
	m.lazy.set("config-history", []config.SnapshotMeta{{ID: "snap1"}}, nil)
	m.syncSelection()
	m.dispatchRowAction('e')
	if m.confirm != nil {
		t.Error("diff is read-only and should not ask for confirmation")
	}
	hasArgs(t, lastCall(f), "system", "config-history", "diff", "snap1")
}

func TestConfigHistoryRestoreArgvNeedsConfirm(t *testing.T) {
	m, f := systemTestModel(t, "config-history")
	m.lazy.set("config-history", []config.SnapshotMeta{{ID: "snap1"}}, nil)
	m.syncSelection()
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("restore should ask for confirmation before overwriting the config")
	}
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "system", "config-history", "restore", "snap1")
}

// ---- power ----------------------------------------------------------------

func TestPowerRestartArgvIsBareWithDelay(t *testing.T) {
	m, f := systemTestModel(t, "power")
	m.lazy.set("power", [2]any{true, ""}, nil)
	openMnemonicForm(t, m, "restart host")
	submitCurrentForm(t, m, map[string]string{"delay": "5"})
	got := lastCall(f)
	hasArgs(t, got, "system", "power", "reboot", "-delay", "5")
	hasNotArgs(t, got, "-config", "-sock")
}

func TestPowerShutdownArgv(t *testing.T) {
	m, f := systemTestModel(t, "power")
	m.lazy.set("power", [2]any{true, ""}, nil)
	openMnemonicForm(t, m, "shutdown host")
	submitCurrentForm(t, m, map[string]string{"delay": "0"})
	hasArgs(t, lastCall(f), "system", "power", "shutdown", "-delay", "0")
}

func TestPowerCancelDoesNothingWhenUnconfirmed(t *testing.T) {
	m, f := systemTestModel(t, "power")
	m.lazy.set("power", [2]any{true, ""}, nil)
	openMnemonicForm(t, m, "cancel pending action")
	submitCurrentForm(t, m, map[string]string{"confirm": "false"})
	if len(f.calls) != 0 {
		t.Errorf("unchecking confirm should not have run anything, got %v", f.calls)
	}
}

func TestPowerCancelArgvWhenConfirmed(t *testing.T) {
	m, f := systemTestModel(t, "power")
	m.lazy.set("power", [2]any{true, ""}, nil)
	openMnemonicForm(t, m, "cancel pending action")
	submitCurrentForm(t, m, map[string]string{"confirm": "true"})
	hasArgs(t, lastCall(f), "system", "power", "cancel")
}

// ---- upgrade --------------------------------------------------------------

func TestUpgradeRollbackArgvIsSockOnly(t *testing.T) {
	m, f := systemTestModel(t, "upgrade")
	openMnemonicForm(t, m, "rollback to the previous binary")
	submitCurrentForm(t, m, map[string]string{"confirm": "true"})
	got := lastCall(f)
	hasArgs(t, got, "upgrade", "rollback", "-sock")
	hasNotArgs(t, got, "-config")
}

func TestUpgradeClearArgv(t *testing.T) {
	m, f := systemTestModel(t, "upgrade")
	openMnemonicForm(t, m, "clear saved rollback state")
	submitCurrentForm(t, m, map[string]string{"confirm": "true"})
	got := lastCall(f)
	hasArgs(t, got, "upgrade", "clear", "-sock")
	hasNotArgs(t, got, "-config")
}

func TestUpgradeHasNoApplyAction(t *testing.T) {
	// Deliberate boundary: apply runs a multi-minute build, incompatible
	// with this console's short mutation timeout (see pageUpgrade's own
	// note). Confirmed absent rather than silently present-but-broken.
	m, _ := systemTestModel(t, "upgrade")
	cards := m.currentCards()
	assignMnemonicsInPlace(cards)
	for _, cd := range cards {
		for _, it := range cd.items {
			if ekv, ok := it.(editableKV); ok {
				for _, row := range ekv.rows {
					if row.k == "apply" || row.k == "check for update" {
						t.Errorf("unexpected apply-like action found: %q", row.k)
					}
				}
			}
		}
	}
}
