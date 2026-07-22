package config

// legacyControlSocket is the control-socket path early versions used on *every*
// platform. "/run" is a Linux (systemd/FHS) convention: it does not exist on a
// stock FreeBSD, macOS or OpenBSD, and isn't even a valid path shape on Windows.
// The per-platform defaults in socket_*.go fixed that — for *new* installs.
//
// They did nothing for existing ones, because Default() writes the then-current
// DefaultControlSocket into the scaffolded config.json (install-*.sh runs
// "gravinet run -init"), so every box installed before that fix has "/run/..."
// frozen into its config file, where it outranks the corrected code default
// forever. The result differed per platform, which is what made it so hard to
// see as one bug:
//
//   - macOS: / is a read-only APFS system volume, so control.Serve's MkdirAll
//     couldn't create /run, net.Listen failed, and the daemon logged one warning
//     and carried on — leaving every CLI command dialing a socket that was never
//     created.
//   - FreeBSD: / is writable, so that same MkdirAll *succeeded* and the daemon
//     manufactured a non-standard top-level /run directory and bound the socket
//     inside it. It works, but only because gravinet fabricated a directory the
//     OS doesn't have — and it entrenches the stale value on disk.
//
// So the stale value is migrated on load rather than merely defaulted around:
// pinning both ends to the platform default keeps the daemon and CLI agreeing by
// construction, instead of depending on whether /run happens to exist on this
// particular box (which, per above, gravinet itself might have created).
const legacyControlSocket = "/run/gravinet.sock"

// NormalizeControlSocket resolves the control-socket endpoint that BOTH the
// daemon (which binds it) and the CLI (which dials it) must use. It returns the
// endpoint and, if the value was rewritten, a note explaining why — worth
// logging once, since it's a config value silently not being honoured verbatim.
//
// Rules, in order:
//   - empty            -> the platform default (what the daemon already did).
//   - legacy "/run/..." on a platform whose default is NOT that (i.e. anything
//     but Linux) -> the platform default. This is the migration described above.
//   - anything else    -> honoured verbatim, including a deliberate /run path on
//     Linux, a custom path anywhere, or a host:port TCP endpoint.
func NormalizeControlSocket(configured string) (endpoint string, note string) {
	if configured == "" {
		return DefaultControlSocket, ""
	}
	if configured == legacyControlSocket && DefaultControlSocket != legacyControlSocket {
		return DefaultControlSocket, "control_socket was " + legacyControlSocket +
			", the pre-v393 Linux-only default frozen into configs scaffolded by older" +
			" versions; /run does not exist on this platform, so using " + DefaultControlSocket +
			" instead. Set control_socket explicitly to override."
	}
	return configured, ""
}
