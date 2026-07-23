//go:build windows

package webadmin

import (
	"strings"
	"syscall"
	"unsafe"

	"gravinet/internal/logx"
	"gravinet/internal/service"
)

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procLogonUserW = advapi32.NewProc("LogonUserW")
)

const (
	logon32LogonNetwork    = 3 // LOGON32_LOGON_NETWORK
	logon32ProviderDefault = 0 // LOGON32_PROVIDER_DEFAULT
)

type winAuth struct {
	log *logx.Logger
}

func systemAuthenticator(pamService string, log *logx.Logger) (Authenticator, bool) {
	_ = pamService
	return &winAuth{log: log}, true
}

func systemAuthName() string { return "windows" }

// PAMCompiledIn is false here — Windows has no PAM. See auth_pam.go's copy
// of this const for why it exists.
const PAMCompiledIn = false

func (a *winAuth) Name() string { return "windows" }

// Authenticate validates credentials against Windows via LogonUserW. Accepts
// "user", "DOMAIN\\user", or "user@domain" (UPN) forms.
func (a *winAuth) Authenticate(user, pass string) bool {
	if user == "" {
		return false
	}
	// Membership in the gravinet local group is the sign-in gate, checked
	// against the bare account name before the domain-prefix parsing below —
	// group membership here only ever applies to local accounts, matching
	// what AddSystemUser/net localgroup manage. (service.IsGroupMember also
	// exempts an account literally named "root" the way it does on Unix;
	// that's simply never a real Windows account name, so the check is
	// harmless dead weight here rather than a meaningful bypass.)
	if !service.IsGroupMember(user) {
		if a.log != nil {
			a.log.Warnf("webadmin/windows: user %q is not root and not a member of the %s group", user, service.GravinetGroup)
		}
		return false
	}

	domain := "." // local machine
	if i := strings.IndexByte(user, '\\'); i >= 0 {
		domain, user = user[:i], user[i+1:]
	} else if strings.IndexByte(user, '@') >= 0 {
		domain = "" // UPN: username carries the domain, pass NULL domain
	}

	pUser, err := syscall.UTF16PtrFromString(user)
	if err != nil {
		return false
	}
	var pDomain *uint16
	if domain != "" {
		if pDomain, err = syscall.UTF16PtrFromString(domain); err != nil {
			return false
		}
	}
	pPass, err := syscall.UTF16PtrFromString(pass)
	if err != nil {
		return false
	}

	var token syscall.Handle
	r, _, lastErr := procLogonUserW.Call(
		uintptr(unsafe.Pointer(pUser)),
		uintptr(unsafe.Pointer(pDomain)),
		uintptr(unsafe.Pointer(pPass)),
		logon32LogonNetwork,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&token)),
	)
	if r != 0 {
		syscall.CloseHandle(token)
		return true
	}
	if a.log != nil {
		a.log.Warnf("webadmin/windows: LogonUser failed for %q: %v", user, lastErr)
	}
	return false
}
