//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package tun

import (
	"errors"
	"net/netip"
)

// errUnsupported is returned on platforms with no overlay-interface backend.
var errUnsupported = errors.New("tun: device backend not implemented on this OS")

// Device is a placeholder on non-Linux platforms so the rest of the tree
// compiles and cross-builds.
type Device struct {
	name string
	mtu  int
}

func New(name string, mtu int) (*Device, error)                  { return nil, errUnsupported }
func (d *Device) Read(p []byte) (int, error)                     { return 0, errUnsupported }
func (d *Device) Write(p []byte) (int, error)                    { return 0, errUnsupported }
func (d *Device) AddIPv4(addr netip.Addr, prefix int) error      { return errUnsupported }
func (d *Device) AddIPv6(addr netip.Addr, prefix int) error      { return errUnsupported }
func (d *Device) AddRoute(prefix netip.Prefix, metric int) error { return errUnsupported }
func (d *Device) DelRoute(prefix netip.Prefix, metric int) error { return errUnsupported }
func (d *Device) Up() error                                      { return errUnsupported }
func (d *Device) Name() string                                   { return d.name }
func (d *Device) MTU() int                                       { return d.mtu }
func (d *Device) Close() error                                   { return nil }
