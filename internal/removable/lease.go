package removable

import (
	"context"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// Lease is exclusive use of one registered disk for the whole span of a
// multi-step operation.
//
// It exists because a repository operation is not a single action. A backup
// mounts a disk, writes to it for a long time, flushes it, and unmounts it, and
// nothing else may touch that disk in between. The per-operation locking the
// media service uses for its own explicit actions cannot express that: each of
// those calls takes and releases the lock, leaving gaps in which another
// operation could mount, unmount, or start its own work.
//
// A lease closes those gaps by holding the same per-disk lock for the caller's
// whole operation, while keeping every media state transition inside this
// package. The holder can ask for a mount, a flush and an unmount, but it never
// learns how any of them is performed and never gets a device node.
type Lease struct {
	service *Service
	diskID  string
	release func()
	// mounted records whether this lease currently believes ROTUP's mount is in
	// place, so a deferred cleanup knows whether there is anything to undo.
	mounted bool
	done    bool
}

// Acquire takes exclusive use of a registered disk. It fails immediately rather
// than waiting: a second operation on the same disk is a request that should be
// refused with an explanation, not queued behind work of unbounded duration.
func (s *Service) Acquire(ctx context.Context, diskID string) (ports.DiskLease, error) {
	if _, err := s.requireDisk(ctx, diskID); err != nil {
		return nil, err
	}
	release, ok := s.locks.tryAcquire(diskID)
	if !ok {
		return nil, busyError()
	}
	return &Lease{service: s, diskID: diskID, release: release}, nil
}

// DiskID returns the leased disk's identifier.
func (l *Lease) DiskID() string { return l.diskID }

// MountPoint returns where this disk is mounted when ROTUP mounts it. It is
// derived from the disk identifier, not from any observation.
func (l *Lease) MountPoint() (string, error) {
	return l.service.mounts.MountPointFor(l.diskID)
}

// Mounted reports whether this lease has a mount in place.
func (l *Lease) Mounted() bool { return l.mounted }

// Mount mounts the disk and proves that what is mounted is the registered
// filesystem. It runs exactly the same checks as the explicit administrator
// mount action, because a backup must not be able to reach media through a
// weaker path than a person can.
func (l *Lease) Mount(ctx context.Context) error {
	if err := l.service.mount(ctx, l.diskID); err != nil {
		return err
	}
	l.mounted = true
	return nil
}

// SyncAndUnmount flushes the filesystem, removes ROTUP's mount, and verifies
// from the kernel mount table that the mount is really gone. A failure here is
// always reported: it is the step that decides whether anything written is
// actually on the media.
func (l *Lease) SyncAndUnmount(ctx context.Context) error {
	err := l.service.syncAndUnmount(ctx, l.diskID)
	// The mount is only still ours if the unmount did not happen. Any outcome
	// other than success leaves the state the media service recorded, and a
	// second attempt from a deferred cleanup would only obscure the first
	// failure.
	l.mounted = false
	return err
}

// Present reports whether the registered filesystem is still connected. It is
// the check that turns a mid-operation failure into an honest "the disk was
// removed" rather than a generic error.
func (l *Lease) Present(ctx context.Context) (bool, error) {
	disk, err := l.service.requireDisk(ctx, l.diskID)
	if err != nil {
		return false, err
	}
	current, err := l.service.observe(ctx)
	if err != nil {
		return false, err
	}
	return len(matchingMedia(current.Media, disk.FilesystemUUID)) == 1, nil
}

// SetStatus records an operational status on the leased disk. Only statuses
// that describe a repository operation are accepted: the media lifecycle states
// belong to the media service, and letting a holder set one would let an
// operation claim a disk is mounted or safe to remove when it is not.
func (l *Lease) SetStatus(ctx context.Context, status domain.DiskStatus) error {
	switch status {
	case domain.DiskBackupRunning, domain.DiskVerifying, domain.DiskMaintenanceRunning, domain.DiskReady:
	default:
		return domain.NewError(domain.ErrClassInvariant, "disk_status_not_operational",
			"only a repository operation status may be set through a disk lease")
	}
	l.service.setStatus(ctx, l.diskID, status, "")
	return nil
}

// MarkError records a disk error code without ending the lease.
func (l *Lease) MarkError(ctx context.Context, errorCode string) {
	l.service.setStatus(ctx, l.diskID, domain.DiskError, errorCode)
}

// Event records a media event against the leased disk.
func (l *Lease) Event(ctx context.Context, severity, eventType string, payload map[string]any) {
	l.service.event(ctx, severity, eventType, l.diskID, payload)
}

// Release ends the lease. It is safe to call more than once.
func (l *Lease) Release() {
	if l.done {
		return
	}
	l.done = true
	l.release()
}

// Compile-time proof that a lease satisfies the port a repository operation
// depends on. The backup service never imports this package.
var _ ports.DiskLease = (*Lease)(nil)
