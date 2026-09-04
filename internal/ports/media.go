package ports

import (
	"context"

	"github.com/bejusxd/rotup/internal/domain"
)

// MediaEnumerator reports the block devices the system currently presents. It
// is authoritative: ROTUP re-enumerates rather than trusting anything it
// remembered, because a device node observed a moment ago may now belong to
// different hardware.
type MediaEnumerator interface {
	Enumerate(ctx context.Context) ([]domain.ObservedMedia, error)
}

// MountRequest names the disk to mount. It deliberately carries no path and no
// mount options: the mount point is derived from the disk identifier by both
// sides of the privileged boundary, and the options are fixed in the
// privileged code.
type MountRequest struct {
	DiskID         string
	FilesystemUUID string
	FilesystemType string
}

// MountResult is what the platform reports after a mount attempt. The caller
// must verify FilesystemUUID against what it asked for before treating the
// disk as usable; a successful call is not by itself proof of identity.
type MountResult struct {
	MountPoint string
	// ResolvedDevice is the node the UUID resolved to during the operation. It
	// is a diagnostic and must never become durable identity.
	ResolvedDevice string
	// FilesystemUUID is read back from the mounted filesystem.
	FilesystemUUID string
	FilesystemType string
	Mounted        bool
}

// DiskLease is exclusive use of one registered disk for the whole span of a
// multi-step repository operation.
//
// It exists because mounting, writing, flushing and unmounting are separate
// steps that must not be interleaved with anything else touching that disk.
// The holder can ask for each step but never learns how any of them is
// performed and never sees a device node: the media lifecycle stays behind this
// interface, so a repository operation cannot reach media by a weaker route
// than an administrator's explicit action does.
type DiskLease interface {
	DiskID() string
	// MountPoint reports where this disk is mounted when ROTUP mounts it. It is
	// derived from the disk identifier, never from an observation.
	MountPoint() (string, error)
	// Mounted reports whether this lease currently has a mount in place.
	Mounted() bool
	// Mount mounts the disk and proves the mounted filesystem is the registered
	// one.
	Mount(ctx context.Context) error
	// SyncAndUnmount flushes the filesystem, removes the mount, and verifies
	// from the kernel mount table that the mount is really gone.
	SyncAndUnmount(ctx context.Context) error
	// Present reports whether the registered filesystem is still connected.
	Present(ctx context.Context) (bool, error)
	// SetStatus records an operational status while the operation runs.
	SetStatus(ctx context.Context, status domain.DiskStatus) error
	// Release ends the lease. It is safe to call more than once.
	Release()
}

// DiskLeaser hands out exclusive use of a registered disk.
type DiskLeaser interface {
	// Acquire fails immediately when the disk is already in use, rather than
	// queueing behind work of unbounded duration.
	Acquire(ctx context.Context, diskID string) (DiskLease, error)
}

// MountManager owns the mount lifecycle of registered media. Every method
// addresses a disk by its ROTUP identifier and its filesystem UUID; none of
// them accepts a path.
type MountManager interface {
	// Available reports whether this platform can manage removable media at
	// all. A false result means every other method returns a typed
	// unsupported-platform error.
	Available() bool
	// MountPointFor returns where a disk would be mounted, for display and for
	// comparing against the authoritative mount table.
	MountPointFor(diskID string) (string, error)
	// Observe returns the authoritative kernel mount table.
	Observe(ctx context.Context) ([]domain.MountObservation, error)
	// Mount mounts the disk below the ROTUP mount root.
	Mount(ctx context.Context, request MountRequest) (MountResult, error)
	// Sync flushes the mounted filesystem to the device.
	Sync(ctx context.Context, diskID string) error
	// Unmount unmounts the ROTUP mount. It never performs a lazy or forced
	// unmount: an unmount that cannot complete cleanly must surface as an
	// error rather than be papered over.
	Unmount(ctx context.Context, diskID string) error
}
