//go:build !linux

package service

// NotifyReady is a no-op on platforms without systemd's sd_notify.
func NotifyReady() error { return nil }
