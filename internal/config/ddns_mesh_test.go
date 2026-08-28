package config

import "testing"

// Overlay addresses are published unless the operator says otherwise. This was
// the other way round for one version, on the reasoning that an overlay address
// answers queries from hosts that cannot route to it — true, and not a reason
// to withhold a record. See v1004.
func TestMeshPublishingDefaultsOn(t *testing.T) {
	var d DDNSConfig
	if !d.MeshEnabled() {
		t.Error("an unset mesh setting excludes the overlay interfaces")
	}
	no := false
	d.Mesh = &no
	if d.MeshEnabled() {
		t.Error("an explicit false was ignored, so the old exclusion cannot be asked for")
	}
	yes := true
	d.Mesh = &yes
	if !d.MeshEnabled() {
		t.Error("an explicit true was ignored")
	}
}

// Reverse and Mesh are both tri-state for the same reason: an explicit choice
// has to survive a later change to what the default means.
func TestDDNSTriStateSettingsKeepAnExplicitOff(t *testing.T) {
	no := false
	d := DDNSConfig{Reverse: &no, Mesh: &no}
	if d.ReverseEnabled() || d.MeshEnabled() {
		t.Error("an explicit off did not survive")
	}
}
