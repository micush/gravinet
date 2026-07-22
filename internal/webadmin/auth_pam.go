//go:build (linux || darwin || freebsd) && cgo

package webadmin

/*
#cgo linux LDFLAGS: -lpam
#cgo darwin LDFLAGS: -lpam
#cgo freebsd LDFLAGS: -lpam
#include <stdlib.h>
#include <string.h>
#include <security/pam_appl.h>

// gravinet_conv answers every PAM prompt with the password carried in
// appdata_ptr. Doing the conversation entirely in C avoids cgo callback and
// pointer-passing rules.
static int gravinet_conv(int num_msg, const struct pam_message **msg,
                         struct pam_response **resp, void *appdata_ptr) {
    if (num_msg <= 0 || num_msg > 32) return PAM_CONV_ERR;
    struct pam_response *r = calloc((size_t)num_msg, sizeof(struct pam_response));
    if (r == NULL) return PAM_BUF_ERR;
    const char *pw = (const char *)appdata_ptr;
    for (int i = 0; i < num_msg; i++) {
        int style = msg[i]->msg_style;
        if (style == PAM_PROMPT_ECHO_OFF || style == PAM_PROMPT_ECHO_ON) {
            r[i].resp = strdup(pw ? pw : "");
            if (r[i].resp == NULL) {
                for (int j = 0; j < i; j++) free(r[j].resp);
                free(r);
                return PAM_BUF_ERR;
            }
        } else {
            r[i].resp = NULL;
        }
        r[i].resp_retcode = 0;
    }
    *resp = r;
    return PAM_SUCCESS;
}

// gravinet_pam_auth returns PAM_SUCCESS or the rc of the first failing step.
static int gravinet_pam_auth(const char *service, const char *user, const char *pass) {
    struct pam_conv conv;
    conv.conv = gravinet_conv;
    conv.appdata_ptr = (void *)pass;
    pam_handle_t *pamh = NULL;
    int rc = pam_start(service, user, &conv, &pamh);
    if (rc != PAM_SUCCESS) return rc;
    rc = pam_authenticate(pamh, 0);
    if (rc == PAM_SUCCESS) {
        rc = pam_acct_mgmt(pamh, 0); // reject expired/disabled accounts
    }
    pam_end(pamh, rc);
    return rc;
}

static const char* gravinet_pam_strerror(int rc) {
    return pam_strerror(NULL, rc); // Linux/macOS/FreeBSD ignore the (NULL) handle here
}
*/
import "C"

import (
	"os"
	"unsafe"

	"gravinet/internal/logx"
)

type pamAuth struct {
	service string
	allow   map[string]bool
	log     *logx.Logger
}

func systemAuthenticator(service string, allow []string, log *logx.Logger) (Authenticator, bool) {
	if service == "" {
		service = "gravinet"
	}
	// Surface the two most common reasons PAM logins fail even when PAM is
	// compiled in, at startup, where an admin will see them.
	if log != nil {
		if _, err := os.Stat("/etc/pam.d/" + service); err != nil {
			log.Warnf("webadmin/pam: service file /etc/pam.d/%s not found — logins will hit PAM's 'other' policy (usually deny). The installer writes this file; create it to use system logins.", service)
		}
		if os.Geteuid() != 0 {
			log.Warnf("webadmin/pam: running as euid=%d, not root — pam_unix usually cannot verify other users' passwords unless the daemon runs as root.", os.Geteuid())
		}
	}
	return &pamAuth{service: service, allow: allowSet(allow), log: log}, true
}

func systemAuthName() string { return "pam" }

// PAMCompiledIn reports whether this binary was actually built with cgo PAM
// support — a fact only the compiler truly knows. Exposed so callers (the
// "version" command in particular) can self-report it, rather than an
// installer script having to infer it after the fact by inspecting the
// binary with ldd/otool/objdump, which is exactly the kind of heuristic
// that can be fooled by static linking or an unusual toolchain. See the
// matching const in auth_nopam.go, auth_other.go, auth_bsdauth.go, and
// auth_windows.go for the non-PAM builds.
const PAMCompiledIn = true

func (a *pamAuth) Name() string { return "pam" }

func (a *pamAuth) Authenticate(user, pass string) bool {
	if user == "" {
		return false
	}
	if a.allow != nil && !a.allow[user] {
		if a.log != nil {
			a.log.Warnf("webadmin/pam: user %q is not in web_admin.allow_users", user)
		}
		return false
	}
	cs := C.CString(a.service)
	cu := C.CString(user)
	cp := C.CString(pass)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cu))
	defer func() {
		if cp != nil {
			C.memset(unsafe.Pointer(cp), 0, C.size_t(len(pass))) // wipe before free
			C.free(unsafe.Pointer(cp))
		}
	}()
	rc := C.gravinet_pam_auth(cs, cu, cp)
	if rc == C.PAM_SUCCESS {
		return true
	}
	if a.log != nil {
		reason := C.GoString(C.gravinet_pam_strerror(rc))
		a.log.Warnf("webadmin/pam: login for %q via service %q rejected: %s (rc=%d)", user, a.service, reason, int(rc))
	}
	return false
}
