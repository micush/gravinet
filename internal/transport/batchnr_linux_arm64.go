package transport

// Linux arm64 syscall numbers for the batched socket calls (the asm-generic
// table, which differs from x86-64's).
const (
	sysRecvmmsg = 243
	sysSendmmsg = 269
)
