// Package unsupported provides removable-media adapters for platforms where
// ROTUP does not manage media. They fail with a typed, honest error rather than
// pretending to work, so a development workstation runs the rest of the daemon
// without ever implying that media operations are available.
package unsupported

import (
	"context"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// ErrorCode is the stable code every operation on this platform reports.
const ErrorCode = "unsupported_platform"

// Reason is the administrator-facing explanation.
const Reason = "ROTUP manages removable media on Linux only; this platform is for development"

func unsupported() error {
	return domain.NewError(domain.ErrClassConfiguration, ErrorCode, Reason)
}

// Enumerator reports no media, without error, so the disks page renders an
// empty observation list rather than an alarming failure.
type Enumerator struct{}

// NewEnumerator returns the no-media enumerator.
func NewEnumerator() *Enumerator { return &Enumerator{} }

// Enumerate always returns no observations.
func (*Enumerator) Enumerate(context.Context) ([]domain.ObservedMedia, error) { return nil, nil }

// MountManager refuses every mount lifecycle operation.
type MountManager struct{}

// NewMountManager returns the refusing mount manager.
func NewMountManager() *MountManager { return &MountManager{} }

// Available always reports false.
func (*MountManager) Available() bool { return false }

// MountPointFor has no answer on this platform.
func (*MountManager) MountPointFor(string) (string, error) { return "", unsupported() }

// Observe reports no mounts.
func (*MountManager) Observe(context.Context) ([]domain.MountObservation, error) { return nil, nil }

// Mount always refuses.
func (*MountManager) Mount(context.Context, ports.MountRequest) (ports.MountResult, error) {
	return ports.MountResult{}, unsupported()
}

// Sync always refuses.
func (*MountManager) Sync(context.Context, string) error { return unsupported() }

// Unmount always refuses.
func (*MountManager) Unmount(context.Context, string) error { return unsupported() }
