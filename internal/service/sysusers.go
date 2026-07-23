package service

// Local console accounts — the backend for the web admin's System > Users
// page, the fourth System item after Upgrade, Time, and Power.
//
// gravinet's system-auth modes (web_admin.auth_mode "pam" on linux/macos/
// freebsd, "system" on openbsd's bsd_auth, and "windows") authenticate real
// OS accounts, and web_admin.allow_users is the gate deciding which of those
// accounts may actually sign in — nil/empty means "any account the OS
// authenticator accepts", a non-empty list means "only these" (see
// config.WebAdmin.AllowUsers and auth_pam.go's pamAuth.Authenticate). That
// list is gravinet's equivalent of the Unix group parapet's Users page
// manages membership of: this file is the OS side (create a login-only
// account, set its password, set or clear its expiry, delete it) and
// handleSystemUsers keeps allow_users in step with it, the same way parapet
// keeps its group membership in step.
//
// This page is deliberately scoped to auth_mode "pam"/"system"/"windows"
// only — never "local". auth_mode "local" authenticates against
// web_admin.users, a list of PBKDF2 hashes gravinet owns directly (see
// config.AdminUser and webadmin/auth.go's GenerateCredential, driven by
// `gravinet genpass`); there's no OS account behind those names at all, so
// nothing here applies to them and this file never touches web_admin.users.
//
// Structure mirrors hosttime.go and power.go: a small typed read
// (ListSystemUsers) plus one function per mutation (AddSystemUser /
// SetSystemUserPassword / SetSystemUserExpiry / DeleteSystemUser), each
// returning (ok, hint), each dispatching on runtime.GOOS. New accounts are
// created login-only — no home directory, and a nologin shell where the
// platform has one — because they exist solely to authenticate to this
// console, nothing else.
//
// Passwords never touch a command line or argv, on any platform: the Unix
// backends pipe "user:password" to chpasswd/pw on stdin (chpasswd on linux,
// pw usermod -h 0 on freebsd), OpenBSD's encrypt(1) is piped a password on
// stdin and its hash piped into usermod -p, macOS's dscl passwd takes the
// password positionally but is spawned with std handles only it can see, and
// the Windows backend hands the password to PowerShell through an
// environment variable rather than a -Command argument, so it doesn't sit in
// the process's own command line either.
//
// Per-platform tooling:
//
//	linux:   useradd/userdel/usermod + chpasswd, matching parapet exactly.
//	darwin:  dscl for create/delete/password. No first-class account expiry —
//	         CanExpiry is false there.
//	freebsd: pw(8) — pw useradd/userdel/usermod, -h 0 for a stdin password.
//	openbsd: useradd/userdel/usermod (base system, not shadow-utils) +
//	         encrypt(1) for a bcrypt hash piped into usermod -p.
//	windows: PowerShell's Microsoft.PowerShell.LocalAccounts module —
//	         New-LocalUser / Set-LocalUser / Remove-LocalUser.
//
// Every external command is exec.Command with separate arguments, never a
// shell string, and every value that reaches one is validated first
// (validUsername / validPassword), so a username or password can't smuggle in
// a second command or break a stdin line a tool parses field-by-field.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SysUser is one entry on the System > Users page: an allow_users name,
// annotated with what's actually true of it on this host.
type SysUser struct {
	Name        string
	Exists      bool      // does an OS account by this name exist right now
	Expires     time.Time // zero = never (or unknown — see ExpiryKnown)
	ExpiryKnown bool      // could expiry be determined for this account
	Expired     bool      // has that expiry already passed (OS has locked it out)
}

// UsersInfo is the full state ListSystemUsers reads for the page.
type UsersInfo struct {
	Users        []SysUser
	Unrestricted bool   // allow_users is empty: any OS account the authenticator accepts may sign in
	CanManage    bool   // can accounts be created/deleted/re-passworded on this host at all
	CanExpiry    bool   // can an account's expiry be set/cleared on this host
	ManageHint   string // why CanManage is false, if it is
	ExpiryHint   string // why CanExpiry is false, if it is
}

// ListSystemUsers reads the live OS state for every name in allow — existence,
// expiry, and whether that expiry has passed — without mutating anything.
// allow is web_admin.allow_users as currently saved; an empty list means no
// restriction, which the page needs to say plainly rather than show an empty
// table that looks broken.
func ListSystemUsers(allow []string) UsersInfo {
	canManage, manageHint := sysUsersCanManage()
	canExpiry, expiryHint := sysUsersCanExpiry()
	info := UsersInfo{
		Unrestricted: len(allow) == 0,
		CanManage:    canManage,
		CanExpiry:    canExpiry,
		ManageHint:   manageHint,
		ExpiryHint:   expiryHint,
	}
	for _, name := range allow {
		u := SysUser{Name: name, Exists: sysUserExists(name)}
		if u.Exists {
			if exp, known := sysUserExpiry(name); known {
				u.ExpiryKnown = true
				u.Expires = exp
				u.Expired = !exp.IsZero() && time.Now().After(exp)
			}
		}
		info.Users = append(info.Users, u)
	}
	return info
}

// sysUserExists uses the standard library's account lookup (getpwnam on Unix,
// the local-account APIs on Windows), so no per-OS code or shelling out is
// needed just to answer "does this account exist".
func sysUserExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

func sysUsersCanManage() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if haveCmd("useradd") && haveCmd("userdel") && haveCmd("chpasswd") {
			return true, ""
		}
		return false, "account management needs useradd/userdel/chpasswd (shadow-utils), not all of which are on this host"
	case "darwin":
		if haveCmd("dscl") {
			return true, ""
		}
		return false, "account management needs dscl, which isn't on this host"
	case "freebsd":
		if haveCmd("pw") {
			return true, ""
		}
		return false, "account management needs pw(8), which isn't on this host"
	case "openbsd":
		if haveCmd("useradd") && haveCmd("userdel") && haveCmd("encrypt") {
			return true, ""
		}
		return false, "account management needs useradd/userdel/encrypt, not all of which are on this host"
	case "windows":
		if haveCmd("powershell") || haveCmd("powershell.exe") {
			return true, ""
		}
		return false, "account management needs PowerShell, which isn't on this host"
	default:
		return false, "account management isn't supported on this operating system"
	}
}

func sysUsersCanExpiry() (bool, string) {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "windows":
		ok, hint := sysUsersCanManage()
		return ok, hint
	case "darwin":
		return false, "macOS accounts have no simple built-in expiry; use Delete when an account should stop working"
	default:
		return false, "account management isn't supported on this operating system"
	}
}

// AddSystemUser creates a login-only account (no home directory, nologin
// shell where the platform has one) with the given password and optional
// expiry (zero = never), or — if the account already exists — just sets its
// password and expiry, leaving it otherwise alone. Mirrors parapet's
// add_user: never recreates an existing account.
func AddSystemUser(name, password string, expires time.Time) (bool, string) {
	if err := validUsername(name); err != nil {
		return false, err.Error()
	}
	if err := validPassword(password); err != nil {
		return false, err.Error()
	}
	if ok, hint := sysUsersCanManage(); !ok {
		return false, hint
	}

	existed := sysUserExists(name)
	if !existed {
		if ok, hint := createOSUser(name); !ok {
			return false, hint
		}
	}
	if ok, hint := setOSPassword(name, password); !ok {
		if !existed {
			// Best-effort cleanup: don't leave a freshly created, unusable
			// account behind if the very first password set failed.
			deleteOSUser(name)
		}
		return false, "account " + verbCreated(existed) + " but setting its password failed: " + hint
	}
	if !expires.IsZero() {
		if ok, hint := SetSystemUserExpiry(name, expires); !ok {
			return true, "account " + verbCreated(existed) + " with password set, but the expiry could not be applied: " + hint
		}
	}
	return true, "user '" + name + "' " + verbCreated(existed)
}

func verbCreated(existed bool) string {
	if existed {
		return "already existed; password updated"
	}
	return "created"
}

// SetSystemUserPassword resets an existing account's password.
func SetSystemUserPassword(name, password string) (bool, string) {
	if err := validUsername(name); err != nil {
		return false, err.Error()
	}
	if err := validPassword(password); err != nil {
		return false, err.Error()
	}
	if ok, hint := sysUsersCanManage(); !ok {
		return false, hint
	}
	if !sysUserExists(name) {
		return false, "no such account '" + name + "'"
	}
	return setOSPassword(name, password)
}

// SetSystemUserExpiry sets (or, with a zero time, clears) an account's
// expiry date. Expiry is whole-day granularity on every backend that
// supports it at all (the tools themselves take a date, not a timestamp), so
// the time-of-day component of expires is dropped.
func SetSystemUserExpiry(name string, expires time.Time) (bool, string) {
	if err := validUsername(name); err != nil {
		return false, err.Error()
	}
	if ok, hint := sysUsersCanExpiry(); !ok {
		return false, hint
	}
	if !sysUserExists(name) {
		return false, "no such account '" + name + "'"
	}
	return setOSExpiry(name, expires)
}

// DeleteSystemUser removes an OS account entirely. The caller (the HTTP
// handler) is responsible for refusing to delete the account the caller is
// currently signed in as — that's a session concern, not an OS one — and for
// keeping allow_users in step; this function only ever touches the OS
// account.
func DeleteSystemUser(name string) (bool, string) {
	if err := validUsername(name); err != nil {
		return false, err.Error()
	}
	if ok, hint := sysUsersCanManage(); !ok {
		return false, hint
	}
	if !sysUserExists(name) {
		// Already gone (e.g. removed by hand). Treat as success so the page
		// can still drop it from allow_users without a confusing error.
		return true, "no OS account '" + name + "' existed"
	}
	return deleteOSUser(name)
}

// ── validation ──────────────────────────────────────────────────────────

// validUsername mirrors parapet's valid_username: POSIX "portable filename"
// style, lowercase, 1–32 chars, starting with a letter or underscore, then
// letters/digits/underscore/hyphen. Deliberately stricter than any one
// platform's own account-name rules, so nothing surprising ever reaches a
// command line, and the same rule works unchanged on every target OS.
func validUsername(name string) error {
	b := []byte(name)
	if len(b) == 0 || len(b) > 32 {
		return errors.New("invalid username: 1\u201332 chars, start with a letter or underscore, then letters, digits, _ or -")
	}
	if !(isLowerAlpha(b[0]) || b[0] == '_') {
		return errors.New("invalid username: 1\u201332 chars, start with a letter or underscore, then letters, digits, _ or -")
	}
	for _, c := range b {
		if !(isLowerAlpha(c) || isDigit(c) || c == '_' || c == '-') {
			return errors.New("invalid username: 1\u201332 chars, start with a letter or underscore, then letters, digits, _ or -")
		}
	}
	if name == "root" || name == "administrator" {
		return fmt.Errorf("refusing to manage the %q account", name)
	}
	return nil
}

func isLowerAlpha(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }

// validPassword rejects an empty password and the characters that would let
// it break out of the "name:password" stdin line chpasswd/pw parse
// field-by-field: a colon would split the field, a newline would smuggle a
// second line in behind it. Harmless-but-consistent on the backends that
// don't share that parsing (dscl, PowerShell, encrypt).
func validPassword(pw string) error {
	if pw == "" {
		return errors.New("password required")
	}
	if len(pw) > 512 {
		return errors.New("password is too long")
	}
	for _, r := range pw {
		if r == '\n' || r == '\r' || r == ':' {
			return errors.New("password may not contain newlines or ':'")
		}
	}
	return nil
}

// ── linux (shadow-utils) ────────────────────────────────────────────────

func linuxNologinShell() string {
	for _, s := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
		if fileExists(s) {
			return s
		}
	}
	return "/bin/false"
}

// ── dispatch ────────────────────────────────────────────────────────────
// createOSUser / setOSPassword / setOSExpiry / deleteOSUser each switch on
// runtime.GOOS and are the only functions above that know which platform
// they're on; every exported function funnels through these four.

func createOSUser(name string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		args := []string{"--no-create-home", "--shell", linuxNologinShell(), name}
		if out, err := exec.Command("useradd", args...).CombinedOutput(); err != nil {
			return false, cmdErr("useradd", out, err)
		}
		return true, ""
	case "darwin":
		return createDarwinUser(name)
	case "freebsd":
		if out, err := exec.Command("pw", "useradd", name, "-m", "-M", "-w", "no", "-s", "/usr/sbin/nologin").CombinedOutput(); err != nil {
			return false, cmdErr("pw useradd", out, err)
		}
		return true, ""
	case "openbsd":
		shell := "/sbin/nologin"
		if !fileExists(shell) {
			shell = "/bin/false"
		}
		if out, err := exec.Command("useradd", "-mNoLogin", "-s", shell, name).CombinedOutput(); err != nil {
			return false, cmdErr("useradd", out, err)
		}
		return true, ""
	case "windows":
		return createWindowsUser(name)
	default:
		return false, "account management isn't supported on this operating system"
	}
}

func setOSPassword(name, password string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if err := runStdin(name+":"+password+"\n", "chpasswd"); err != nil {
			return false, err.Error()
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("dscl", ".", "-passwd", "/Users/"+name, password).CombinedOutput(); err != nil {
			return false, cmdErr("dscl -passwd", out, err)
		}
		return true, ""
	case "freebsd":
		if err := runStdin(password+"\n", "pw", "usermod", name, "-h", "0"); err != nil {
			return false, err.Error()
		}
		return true, ""
	case "openbsd":
		hash, err := runOutStdin(password+"\n", "encrypt", "-b", "8")
		if err != nil {
			return false, cmdErr("encrypt", nil, err)
		}
		if out, err := exec.Command("usermod", "-p", hash, name).CombinedOutput(); err != nil {
			return false, cmdErr("usermod -p", out, err)
		}
		return true, ""
	case "windows":
		return setWindowsPassword(name, password)
	default:
		return false, "account management isn't supported on this operating system"
	}
}

func setOSExpiry(name string, expires time.Time) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		arg := ""
		if !expires.IsZero() {
			arg = expires.UTC().Format("2006-01-02")
		}
		if out, err := exec.Command("usermod", "--expiredate", arg, name).CombinedOutput(); err != nil {
			return false, cmdErr("usermod --expiredate", out, err)
		}
	case "freebsd":
		arg := "0"
		if !expires.IsZero() {
			arg = expires.UTC().Format("2006-01-02")
		}
		if out, err := exec.Command("pw", "usermod", name, "-e", arg).CombinedOutput(); err != nil {
			return false, cmdErr("pw usermod -e", out, err)
		}
	case "openbsd":
		arg := ""
		if !expires.IsZero() {
			arg = expires.UTC().Format("2006-01-02")
		}
		if out, err := exec.Command("usermod", "-e", arg, name).CombinedOutput(); err != nil {
			return false, cmdErr("usermod -e", out, err)
		}
	case "windows":
		return setWindowsExpiry(name, expires)
	default:
		return false, "account expiry isn't supported on this operating system"
	}
	if expires.IsZero() {
		return true, "expiry cleared for '" + name + "'"
	}
	return true, "'" + name + "' set to expire " + expires.UTC().Format("2006-01-02")
}

func deleteOSUser(name string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("userdel", name).CombinedOutput(); err != nil {
			return false, cmdErr("userdel", out, err)
		}
	case "darwin":
		if out, err := exec.Command("dscl", ".", "-delete", "/Users/"+name).CombinedOutput(); err != nil {
			return false, cmdErr("dscl -delete", out, err)
		}
	case "freebsd":
		if out, err := exec.Command("pw", "userdel", name).CombinedOutput(); err != nil {
			return false, cmdErr("pw userdel", out, err)
		}
	case "openbsd":
		if out, err := exec.Command("userdel", name).CombinedOutput(); err != nil {
			return false, cmdErr("userdel", out, err)
		}
	case "windows":
		return deleteWindowsUser(name)
	default:
		return false, "account management isn't supported on this operating system"
	}
	return true, "user '" + name + "' deleted"
}

// ── linux/freebsd/openbsd shared exec helpers ──────────────────────────
// (darwin and windows go through the same exec.Command calls above/below;
// these two just need stdin piping, which the *nix branches above share.)

// runStdin runs name(args...) with input fed to its stdin — used to hand
// passwords to chpasswd/pw without ever placing them on the command line.
func runStdin(input, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(cmdErr(name, out, err))
	}
	return nil
}

// runOutStdin is runStdin but also captures stdout — for OpenBSD's encrypt(1),
// which reads the password on stdin and writes the resulting hash to stdout.
func runOutStdin(input, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// ── darwin (dscl) ───────────────────────────────────────────────────────

// createDarwinUser creates a login-only Directory Service user: a fresh
// UniqueID above the existing range, /var/empty for a home (never created),
// and /usr/bin/false as the shell so the account can authenticate but never
// gets an interactive login of its own.
func createDarwinUser(name string) (bool, string) {
	path := "/Users/" + name
	steps := [][]string{
		{"-create", path},
		{"-create", path, "UserShell", "/usr/bin/false"},
		{"-create", path, "NFSHomeDirectory", "/var/empty"},
	}
	uid, hint := nextDarwinUID()
	if hint != "" {
		return false, hint
	}
	steps = append(steps, []string{"-create", path, "UniqueID", uid})
	steps = append(steps, []string{"-create", path, "PrimaryGroupID", "20"}) // staff
	for _, args := range steps {
		full := append([]string{"."}, args...)
		if out, err := exec.Command("dscl", full...).CombinedOutput(); err != nil {
			deleteOSUser(name) // best-effort cleanup of a partially created record
			return false, cmdErr("dscl "+args[0], out, err)
		}
	}
	return true, ""
}

// nextDarwinUID picks a UniqueID one above the current highest, starting no
// lower than 501 (macOS's usual first non-system UID), by listing every
// existing account's UniqueID via dscl and taking the max.
func nextDarwinUID() (string, string) {
	out, err := exec.Command("dscl", ".", "-list", "/Users", "UniqueID").CombinedOutput()
	if err != nil {
		return "", cmdErr("dscl -list", out, err)
	}
	max := 500
	for _, ln := range strings.Split(string(out), "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1), ""
}

// ── windows (PowerShell LocalAccounts) ─────────────────────────────────

// psRun runs a PowerShell command, returning a cleaned-up error on failure.
// Secrets that need to reach the script (only the password, currently) go in
// via an environment variable named in envPairs, never interpolated into the
// script text itself — so a password can't break out into a second command
// even if it happened to contain PowerShell metacharacters, and never shows
// up in the process's own argv.
func psRun(script string, envPairs ...string) (string, error) {
	shell := "powershell"
	if !haveCmd(shell) {
		shell = "powershell.exe"
	}
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), envPairs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New(cmdErr("powershell", out, err))
	}
	return string(out), nil
}

func createWindowsUser(name string) (bool, string) {
	// AccountNeverExpires here is about Windows' own separate "account
	// expires" concept only in the sense of leaving it at the default (never)
	// at creation time; SetSystemUserExpiry (setWindowsExpiry) is what
	// actually manages it afterwards, via net user /expires so a later change
	// doesn't require re-creating the account.
	script := `New-LocalUser -Name '` + name + `' -NoPassword -AccountNeverExpires -UserMayNotChangePassword -PasswordNeverExpires; Set-LocalUser -Name '` + name + `' -PasswordExpirationTime $null`
	if _, err := psRun(script); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func setWindowsPassword(name, password string) (bool, string) {
	script := `$sec = ConvertTo-SecureString $env:GRAVINET_NEWPW -AsPlainText -Force; Set-LocalUser -Name '` + name + `' -Password $sec`
	if _, err := psRun(script, "GRAVINET_NEWPW="+password); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func setWindowsExpiry(name string, expires time.Time) (bool, string) {
	arg := "NEVER"
	human := "cleared"
	if !expires.IsZero() {
		arg = expires.UTC().Format("01/02/2006")
		human = "set to " + expires.UTC().Format("2006-01-02")
	}
	if out, err := exec.Command("net", "user", name, "/expires:"+arg).CombinedOutput(); err != nil {
		return false, cmdErr("net user /expires", out, err)
	}
	return true, "expiry " + human + " for '" + name + "'"
}

func deleteWindowsUser(name string) (bool, string) {
	script := `Remove-LocalUser -Name '` + name + `'`
	if _, err := psRun(script); err != nil {
		return false, err.Error()
	}
	return true, "user '" + name + "' deleted"
}

// ── expiry reads ────────────────────────────────────────────────────────

// sysUserExpiry reads an account's current expiry, returning (expiry-or-zero,
// known). known is false whenever the host doesn't expose expiry at all
// (darwin) or the read itself failed (e.g. /etc/shadow unreadable without
// root) — distinct from "known and set to never", which is (zero, true).
func sysUserExpiry(name string) (time.Time, bool) {
	switch runtime.GOOS {
	case "linux":
		return linuxShadowExpiry(name)
	case "freebsd":
		return freebsdExpiry(name)
	case "openbsd":
		return openbsdExpiry(name)
	case "windows":
		return windowsExpiry(name)
	default:
		return time.Time{}, false
	}
}

// shadowPath is /etc/shadow, as a variable so tests can point it at a fixture
// instead of the real file (which usually isn't even readable as this test
// runs unprivileged).
var shadowPath = "/etc/shadow"

// linuxShadowExpiry reads field 8 (0-indexed 7) of /etc/shadow — days since
// the epoch, empty meaning never — the same field parapet reads.
func linuxShadowExpiry(name string) (time.Time, bool) {
	b, err := os.ReadFile(shadowPath)
	if err != nil {
		return time.Time{}, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Split(ln, ":")
		if len(f) < 8 || f[0] != name {
			continue
		}
		e := strings.TrimSpace(f[7])
		if e == "" {
			return time.Time{}, true
		}
		days, err := strconv.ParseInt(e, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(days*86400, 0).UTC(), true
	}
	return time.Time{}, false
}

// freebsdExpiry reads `pw usershow -7`'s colon-separated fields (chpass -l
// format): name:password:uid:gid:class:change:expire:gecos:home:shell — expire
// is field index 6, a Unix timestamp in seconds, "0" meaning never.
func freebsdExpiry(name string) (time.Time, bool) {
	out, err := exec.Command("pw", "usershow", name, "-7").CombinedOutput()
	if err != nil {
		return time.Time{}, false
	}
	return parseFreeBSDUsershow(string(out))
}

func parseFreeBSDUsershow(out string) (time.Time, bool) {
	f := strings.Split(strings.TrimSpace(out), ":")
	if len(f) < 7 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(f[6], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if secs == 0 {
		return time.Time{}, true
	}
	return time.Unix(secs, 0).UTC(), true
}

// openbsdExpiry parses `userinfo`'s human-readable output for an "Expiration
// date" line. userinfo's own display format for a set date is
// "Www Mmm dd hh:mm:ss yyyy" (Go reference: "Mon Jan 2 15:04:05 2006"); an
// unset expiry prints "Never" or is omitted, both treated as "known, never".
func openbsdExpiry(name string) (time.Time, bool) {
	out, err := exec.Command("userinfo", name).CombinedOutput()
	if err != nil {
		return time.Time{}, false
	}
	return parseOpenBSDUserinfo(string(out))
}

// parseOpenBSDUserinfo parses userinfo's tab-separated "label\tvalue" output
// for an expiration line. Deliberately does not fall back to splitting on the
// first colon: userinfo's date value itself contains colons (HH:MM:SS), so a
// naive colon-split would slice into the time rather than the label
// separator. A line matching "expir" without a tab is treated as an
// unrecognized format (known=false) rather than guessed at.
func parseOpenBSDUserinfo(out string) (time.Time, bool) {
	for _, ln := range strings.Split(out, "\n") {
		lower := strings.ToLower(ln)
		if !strings.Contains(lower, "expir") {
			continue
		}
		tab := strings.IndexByte(ln, '\t')
		if tab < 0 {
			return time.Time{}, false
		}
		val := strings.TrimSpace(ln[tab+1:])
		if val == "" || strings.EqualFold(val, "never") {
			return time.Time{}, true
		}
		if t, err := time.Parse("Mon Jan 2 15:04:05 2006", val); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	}
	return time.Time{}, true // no expiration line at all == never
}

func windowsExpiry(name string) (time.Time, bool) {
	out, err := exec.Command("net", "user", name).CombinedOutput()
	if err != nil {
		return time.Time{}, false
	}
	return parseWindowsNetUser(string(out))
}

// parseWindowsNetUser parses `net user`'s "Account expires" line. net user
// pads the label to a fixed column width with spaces — there is no colon
// separator — and the value itself contains colons (a time-of-day), so this
// strips the known label text directly rather than splitting on the first
// colon, which would slice into the timestamp instead.
func parseWindowsNetUser(out string) (time.Time, bool) {
	const label = "Account expires"
	ln := lineContaining(out, label)
	if ln == "" {
		return time.Time{}, false
	}
	i := strings.Index(ln, label)
	val := strings.TrimSpace(ln[i+len(label):])
	if val == "" || strings.EqualFold(val, "Never") {
		return time.Time{}, true
	}
	for _, layout := range []string{"1/2/2006 3:04:05 PM", "1/2/2006"} {
		if t, err := time.Parse(layout, val); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
