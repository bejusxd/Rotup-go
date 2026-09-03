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
