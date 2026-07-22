//go:build darwin

package mesh

// platformPMTUCeil caps path-MTU discovery's ceiling on macOS.
//
// internal/transport/control_darwin.go sets IP_DONTFRAG/IPV6_DONTFRAG so that
// an oversized probe fails fast with EMSGSIZE instead of being silently
// IP-fragmented — that's what lets a successful probe in pmtu.go mean "this
// exact datagram crossed the path as one piece," not "some IP fragments of it
// happened to survive." On real macOS that setsockopt is well documented to
// fail silently with ENOPROTOOPT even though the constant is defined
// correctly in the SDK headers (see e.g. github.com/NLnetLabs/unbound#347),
// so the don't-fragment guarantee never actually holds here, and it's ignored
// as best-effort rather than surfaced.
//
// Without it, discovery can settle on a size that only worked because a
// benign/local test path tolerated IP fragmentation, then black-hole once
// real traffic crosses a path (home router, corporate firewall, mobile NAT,
// cloud VPC) that drops fragments outright — exactly the failure mode
// internal/mesh/frag.go's application-layer fragmentation exists to avoid in
// the first place. A peer stuck on a too-large discovered size doesn't error;
// its packets just stop arriving, so the session eventually goes quiet and
// the peer ages out of the mesh registry (and any manageable-peer list) with
// no obvious cause.
//
// Capping the ceiling below any realistic single-datagram path MTU sidesteps
// the problem entirely: discovery still finds the best size up to this cap,
// but never has to trust fragmented delivery to validate a probe.
const platformPMTUCeil = 1400
