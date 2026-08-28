package config

import "testing"

// Reverse is tri-state so an operator's explicit off survives a later change
// to what the default means. It is the last setting in this block that needs
// to be: Mesh was the other one, and v1005 removed it — every interface is
// published now, including the overlay devices, and nothing says otherwise.
func TestReverseKeepsAnExplicitOff(t *testing.T) {
	no := false
	if (DDNSConfig{Reverse: &no}).ReverseEnabled() {
		t.Error("an explicit off did not survive")
	}
	if !(DDNSConfig{}).ReverseEnabled() {
		t.Error("an unset reverse setting stopped publishing PTRs")
	}
}
