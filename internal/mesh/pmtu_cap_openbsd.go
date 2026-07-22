//go:build openbsd

package mesh

// platformPMTUCeil caps path-MTU discovery's ceiling on OpenBSD, for a
// different reason than darwin's (see pmtu_cap_darwin.go): OpenBSD's IPv4
// stack has no IP_DONTFRAG socket option at all — it isn't in ip(4)'s
// documented option list, and Go's own syscall package doesn't export the
// constant for this GOOS, unlike freebsd and windows (see
// internal/transport/control_openbsd.go). IPV6_DONTFRAG does exist and is
// set for v6 sockets, so this is only strictly needed for the v4 path; the
// cap here is a single ceiling shared across both (same as darwin's), which
// is a safe, if slightly conservative, choice for v6 rather than adding a
// second, family-specific ceiling for one platform's one-sided gap.
//
// Without a cap, v4 discovery can settle on a size that only worked because
// the kernel silently fragmented an oversized probe rather than rejecting
// it — see pmtu_cap_darwin.go's doc comment for the failure mode this avoids.
const platformPMTUCeil = 1400
