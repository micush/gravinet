//go:build (linux || darwin) && !cgo

package webadmin

import "gravinet/internal/logx"

// Real PAM needs cgo + libpam. In a pure CGO_ENABLED=0 build it isn't available,
// so callers fall back to local users.
func systemAuthenticator(service string, log *logx.Logger) (Authenticator, bool) {
	if log != nil {
		log.Warnf("webadmin: this binary was built without cgo, so PAM is not available — rebuild with CGO_ENABLED=1 (needs libpam headers) for system logins.")
	}
	return nil, false
}

func systemAuthName() string { return "pam" }

// PAMCompiledIn is false here — see auth_pam.go's copy of this const for why
// it exists.
const PAMCompiledIn = false
