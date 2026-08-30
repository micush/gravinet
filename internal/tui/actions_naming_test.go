package tui

import (
	"testing"

	"gravinet/internal/config"
)

func namingTestSnapshot() *snapshot {
	s := testSnapshot()
	s.cfg.Networks[0].DNSAdvertise = []config.DNSForward{
		{Domain: "corp.example", Servers: []string{"10.42.0.1"}},
	}
	s.cfg.Networks[0].DNSReject = []config.DNSReject{
		{Domain: "evil.example"},
	}
	s.cfg.Networks[0].HostsAdvertise = []config.HostRecord{
		{Name: "printer", IP: "10.42.0.50"},
	}
	return s
}

func namingTestModel(t *testing.T, section string) (*model, *fakeGravinet) {
	t.Helper()
	f := installFakeGravinet(t)
	m := newModel(namingTestSnapshot(), "dark", colorMono)
	m.w, m.h = 120, 40
	m.setSection(section)
	return m, f
}

// ---- dns ----------------------------------------------------------------

func TestDNSAddForwardArgv(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"net": "corp", "domain": "internal.example", "servers": "10.42.0.1,10.42.0.2"})
	hasArgs(t, lastCall(f), "naming", "dns", "add", "internal.example", "10.42.0.1,10.42.0.2", "-net", "corp")
}

func TestDNSForwardEditIsAnUpsertAdd(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.dispatchRowAction('e')
	if m.form == nil {
		t.Fatal("'e' should open the edit form")
	}
	submitCurrentForm(t, m, map[string]string{"servers": "10.42.0.9"})
	hasArgs(t, lastCall(f), "naming", "dns", "add", "corp.example", "10.42.0.9", "-net", "corp")
}

func TestDNSForwardDeleteArgv(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "naming", "dns", "remove", "corp.example", "-net", "corp")
}

func TestDNSForwardToggleArgv(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "naming", "dns", "disable", "corp.example", "-net", "corp")
}

func TestDNSRejectRowsHaveNoEditActionOnlyToggleAndDelete(t *testing.T) {
	m, _ := namingTestModel(t, "dns")
	m.selTable, m.selID = "dns-reject", "corp"+idSep+"evil.example"
	if legend := m.actionLegend(); legend == "" {
		t.Fatal("reject row should offer at least delete/toggle")
	}
	m.dispatchRowAction('e')
	if m.form != nil {
		t.Error("reject rows have no edit form (there is nothing to edit but the domain itself)")
	}
}

func TestDNSRejectToggleArgv(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.selTable, m.selID = "dns-reject", "corp"+idSep+"evil.example"
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "naming", "dns", "reject-disable", "evil.example", "-net", "corp")
}

func TestDNSRejectDeleteArgv(t *testing.T) {
	m, f := namingTestModel(t, "dns")
	m.selTable, m.selID = "dns-reject", "corp"+idSep+"evil.example"
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "naming", "dns", "reject-remove", "evil.example", "-net", "corp")
}

// ---- hosts ----------------------------------------------------------------

func TestHostsAddArgv(t *testing.T) {
	m, f := namingTestModel(t, "hosts")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"net": "corp", "name": "nas", "ip": "10.42.0.60"})
	hasArgs(t, lastCall(f), "naming", "hosts", "add", "nas", "10.42.0.60", "-net", "corp")
}

func TestHostsEditIsAnUpsertAdd(t *testing.T) {
	m, f := namingTestModel(t, "hosts")
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"ip": "10.42.0.99"})
	hasArgs(t, lastCall(f), "naming", "hosts", "add", "printer", "10.42.0.99", "-net", "corp")
}

func TestHostsDeleteArgv(t *testing.T) {
	m, f := namingTestModel(t, "hosts")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "naming", "hosts", "remove", "printer", "-net", "corp")
}

func TestHostsToggleArgv(t *testing.T) {
	m, f := namingTestModel(t, "hosts")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "naming", "hosts", "disable", "printer", "-net", "corp")
}
