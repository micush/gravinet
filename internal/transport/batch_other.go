//go:build !linux || (!amd64 && !arm64)

// Everywhere the batched fast path is not compiled in — every non-Linux
// platform, and 32-bit Linux, where struct msghdr's layout differs and could
// not be verified here — the transport keeps its original per-packet
// behaviour exactly. These are the no-op seams that let transport.go stay
// platform-neutral: reads go straight to readLoop, and Send finds no ring
// (rings4/rings6 stay nil) so it takes the direct write it always took.

package transport

import "net"

// batchAvailable reports that this build has no batched path compiled in.
const batchAvailable = false

// readLoopBatched is the plain per-packet reader here. readLoop owns the
// wg.Done, matching the Linux variant's contract.
func (t *Transport) readLoopBatched(c *net.UDPConn, fam Family) { t.readLoop(c, fam) }

// initBatch does nothing: no rings are created, so Send's ring lookup always
// misses and every datagram takes the direct path.
func (t *Transport) initBatch() {}

// stopBatch does nothing: there are no flushers to stop.
func (t *Transport) stopBatch() {}
