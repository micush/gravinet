package config

import "testing"

func TestForwardingEnabledDefault(t *testing.T) {
	if !(&Config{}).ForwardingEnabled() {
		t.Error("unset ip_forwarding should default to enabled")
	}
	f := false
	if (&Config{IPForwarding: &f}).ForwardingEnabled() {
		t.Error("ip_forwarding:false should disable")
	}
	t2 := true
	if !(&Config{IPForwarding: &t2}).ForwardingEnabled() {
		t.Error("ip_forwarding:true should enable")
	}
}
