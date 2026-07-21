package transport

// Linux x86-64 syscall numbers for the batched socket calls. The stdlib
// syscall package defines SYS_RECVMMSG here but not SYS_SENDMMSG, so both are
// spelled out locally rather than half-imported.
const (
	sysRecvmmsg = 299
	sysSendmmsg = 307
)
