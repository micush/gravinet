//go:build !linux && !darwin && !windows && !openbsd && !(freebsd && cgo)

package webadmin

import "gravinet/internal/logx"

// No system authenticator on this platform; callers fall back to local users.
func systemAuthenticator(service string, log *logx.Logger) (Authenticator, bool) {
	return nil, false
}

func systemAuthName() string { return "system" }

// PAMCompiledIn is false here — see auth_pam.go's copy of this const for why
// it exists.
const PAMCompiledIn = false
