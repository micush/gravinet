//go:build openbsd

package config

// OpenBSD has no PAM; its system authenticator is bsd_auth (see
// webadmin/auth_bsdauth.go). The web-admin auth-mode switch routes the generic
// "system" value to that backend, so that — not "pam" — is the honest default
// here. ("pam" would also route there today, but would misdescribe what's
// actually doing the authenticating.)
func defaultAuthMode() string { return "system" }
