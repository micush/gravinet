//go:build openbsd

package webadmin

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"gravinet/internal/logx"
	"gravinet/internal/service"
)

// loginPasswdPath is OpenBSD's bsd_auth(3) password login helper — the same
// program login(1) and su(1) drive. Shelling out to it is the no-cgo
// equivalent of calling auth_userokay(3): it keeps gravinet's CGO_ENABLED=0
// static build (OpenBSD has no PAM, so there's no auth_pam.go path here) while
// still authenticating real system accounts.
const loginPasswdPath = "/usr/libexec/auth/login_passwd"

// authBackChannelFD is the descriptor a bsd_auth login script reads its
// challenge/response from and writes its result to. bsd_auth fixes this at fd
// 3 (the login helpers do fdopen(3, "r+")); cmd.ExtraFiles[0] lands there
// because the child already has 0,1,2. This fd-3 convention is the main
// untested-on-hardware assumption in this file — see loginPasswdVerify.
const authBackChannelFD = 3

type bsdAuth struct {
	log *logx.Logger
}

// systemAuthenticator returns the OpenBSD bsd_auth backend. The PAM "service"
// argument is meaningless here (bsd_auth uses login classes in login.conf,
// not per-service policy files), so it's accepted only for signature parity
// with the PAM/Windows backends and ignored.
func systemAuthenticator(pamService string, log *logx.Logger) (Authenticator, bool) {
	_ = pamService
	if log != nil {
		if _, err := os.Stat(loginPasswdPath); err != nil {
			log.Warnf("webadmin/bsd_auth: %s not found — system logins can't work on this host; set web_admin.auth_mode=\"local\" and add a user with 'gravinet genpass'.", loginPasswdPath)
		}
		// login_passwd verifies against the master password database, which
		// only root can read — same practical constraint PAM's pam_unix has.
		if os.Geteuid() != 0 {
			log.Warnf("webadmin/bsd_auth: running as euid=%d, not root — login_passwd usually cannot verify other users' passwords unless the daemon runs as root.", os.Geteuid())
		}
	}
	return &bsdAuth{log: log}, true
}

func systemAuthName() string { return "bsd_auth" }

// PAMCompiledIn is false here — OpenBSD authenticates via bsd_auth
// (login_passwd(8)), not PAM. See auth_pam.go's copy of this const for why
// it exists.
const PAMCompiledIn = false

func (a *bsdAuth) Name() string { return "bsd_auth" }

func (a *bsdAuth) Authenticate(user, pass string) bool {
	if user == "" {
		return false
	}
	// Membership in the gravinet OS group is the sign-in gate — root is the
	// only exception, exactly like a normal Unix login. Checked before
	// shelling out to login_passwd: no reason to run an external helper for
	// a correct password on an account that was never going to be let in.
	if !service.IsGroupMember(user) {
		if a.log != nil {
			a.log.Warnf("webadmin/bsd_auth: user %q is not root and not a member of the %s group", user, service.GravinetGroup)
		}
		return false
	}
	ok, err := loginPasswdVerify(user, pass)
	if err != nil {
		if a.log != nil {
			a.log.Warnf("webadmin/bsd_auth: login for %q failed: %v", user, err)
		}
		return false
	}
	if !ok && a.log != nil {
		a.log.Warnf("webadmin/bsd_auth: login for %q rejected", user)
	}
	return ok
}

// loginPasswdVerify authenticates user/pass through login_passwd's "response"
// service, which takes credentials over the back channel rather than a tty.
//
// Protocol (login.conf(5), login_passwd(8), OpenBSD's auth_call):
//   - Spawn: login_passwd -s response <user>, with a socketpair handed to the
//     child as fd 3 (see authBackChannelFD).
//   - Write the auth data to that fd as two NUL-terminated strings: the
//     challenge (empty here) followed by the password. Then half-close the
//     write side so the child's single read sees the whole message and EOF.
//   - The helper writes result directives back on the same fd; success is the
//     token "authorize" (bsd_auth's BI_AUTH). We require both that token and a
//     zero exit, and treat anything else as a plain rejection rather than an
//     error, since a wrong password legitimately produces a non-zero exit.
func loginPasswdVerify(user, pass string) (bool, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false, err
	}
	parent := os.NewFile(uintptr(fds[0]), "bsdauth-back-parent")
	child := os.NewFile(uintptr(fds[1]), "bsdauth-back-child")
	defer parent.Close()

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		child.Close()
		return false, err
	}
	defer devnull.Close()

	cmd := exec.Command(loginPasswdPath, "-s", "response", user)
	// login_passwd's "response" service uses only the back channel, not a tty,
	// so std streams go to /dev/null to keep it from touching a controlling
	// terminal it doesn't have.
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.ExtraFiles = []*os.File{child} // becomes fd 3 in the child

	if err := cmd.Start(); err != nil {
		child.Close()
		return false, err
	}
	child.Close() // parent keeps only its own end

	// "<challenge>\0<password>\0" with an empty challenge.
	buf := make([]byte, 0, len(pass)+2)
	buf = append(buf, 0)
	buf = append(buf, pass...)
	buf = append(buf, 0)
	_, werr := parent.Write(buf)
	for i := range buf { // wipe our copy of the password bytes
		buf[i] = 0
	}
	// Half-close the write direction: the helper does a single read and must
	// see EOF to stop waiting for more, without us closing the read side we
	// still need for the result.
	if werr == nil {
		_ = syscall.Shutdown(int(parent.Fd()), syscall.SHUT_WR)
	}

	authorized := false
	if werr == nil {
		sc := bufio.NewScanner(parent)
		for sc.Scan() {
			// Directives are whitespace-separated; the keyword is the first
			// field ("authorize" / "authorize root" => ok; "reject" / "reject
			// silent" => not).
			field := strings.TrimSpace(sc.Text())
			if i := strings.IndexByte(field, ' '); i >= 0 {
				field = field[:i]
			}
			switch field {
			case "authorize":
				authorized = true
			case "reject":
				authorized = false
			}
		}
	}

	waitErr := cmd.Wait()
	if werr != nil {
		return false, werr
	}
	if waitErr != nil {
		return false, nil // non-zero exit == rejected, not a hard error
	}
	return authorized, nil
}
