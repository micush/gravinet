package config

import "testing"

func TestRedirectsDisabledDefault(t *testing.T) {
	if !(&Config{}).RedirectsDisabled() {
		t.Error("unset disable_redirects should default to disabled (redirects off)")
	}
	f := false
	if (&Config{DisableRedirects: &f}).RedirectsDisabled() {
		t.Error("disable_redirects:false should leave host redirect settings untouched")
	}
	t2 := true
	if !(&Config{DisableRedirects: &t2}).RedirectsDisabled() {
		t.Error("disable_redirects:true should disable redirects")
	}
}
