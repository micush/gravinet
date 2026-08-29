package tui

import (
	"testing"

	"gravinet/internal/mesh"
)

func TestDispatchAddOpensTheRegisteredForm(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.dispatchAdd()
	if m.form == nil {
		t.Fatal("'a' on networks should have opened the add form")
	}
	if m.form.spec.title != "add network" {
		t.Errorf("wrong form opened: %q", m.form.spec.title)
	}
}

func TestDispatchAddOnASectionWithNoAddFormSaysSo(t *testing.T) {
	m := testModel()
	m.setSection("about") // a read-only page with no registered actions at all
	m.dispatchAdd()
	if m.form != nil {
		t.Fatal("a form opened on a page with nothing to add")
	}
	if m.flash == "" {
		t.Error("dispatchAdd was silent instead of explaining why nothing happened")
	}
}

func TestDispatchRowActionRunsAConfirmForDelete(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	// The test snapshot's one network, "corp", is what syncSelection lands
	// the cursor on by default.
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("'d' on a network should ask for confirmation before deleting")
	}
}

func TestDispatchRowActionWithNoSelectionSaysSo(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.selTable, m.selID = "", ""
	m.dispatchRowAction('d')
	if m.confirm != nil || m.form != nil {
		t.Error("an action ran with nothing selected")
	}
	if m.flash == "" {
		t.Error("dispatchRowAction was silent instead of explaining why nothing happened")
	}
}

func TestDispatchRowActionForAnUnregisteredKeySaysSo(t *testing.T) {
	m := testModel()
	m.setSection("networks")
	m.dispatchRowAction('z')
	if m.flash == "" {
		t.Error("an unbound row-action key should explain nothing happened, not do nothing silently")
	}
}

func TestActionLegendReflectsTheSelectedRowNotAStaticList(t *testing.T) {
	// Bans only offers 'd' on a row this node itself issued (see
	// bansActions' row func) — the legend for a not-mine ban must not
	// advertise a delete that would just fail.
	m := testModel()
	m.snap.bans = []liveBan{{net: "corp", BanInfo: mesh.BanInfo{Target: "notmine", Mine: false}}}
	m.setSection("bans")
	m.selTable, m.selID = "bans", "corp"+idSep+"notmine"
	if legend := m.actionLegend(); legend != "" {
		t.Errorf("legend for a not-mine ban should be empty, got %q", legend)
	}

	m.snap.bans = []liveBan{{net: "corp", BanInfo: mesh.BanInfo{Target: "mine", Mine: true}}}
	m.selID = "corp" + idSep + "mine"
	if legend := m.actionLegend(); legend == "" {
		t.Error("legend for this node's own ban should offer delete")
	}
}
