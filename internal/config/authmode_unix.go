//go:build !windows && !openbsd

package config

func defaultAuthMode() string { return "pam" }
