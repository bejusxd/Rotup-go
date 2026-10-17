// Package coordinator acquires the existing in-process resources for scheduled
// work. It owns no database state, device paths or mount policy.
package coordinator

import (
	"context"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/staging"
)

// SourceLocker is implemented by the collection service, sharing its manual
// operation locks rather than creating a second source-exclusion mechanism.
type SourceLocker interface {
	TryAcquireSource(sourceID string) (func(), error)
}

// Coordinator uses a fixed order: staging-mutator, source, disk. Current
// operations need at most one source lock or one disk lease, so a generic lock
// registry or caller-supplied resource ordering is unnecessary.
type Coordinator struct {
	barrier *staging.Barrier
	sources SourceLocker
	disks   ports.DiskLeaser
}

func New(barrier *staging.Barrier, sources SourceLocker, disks ports.DiskLeaser) *Coordinator {
	return &Coordinator{barrier: barrier, sources: sources, disks: disks}
}

// TryCollection grants the whole set or releases everything on refusal. It
// never starts collection or creates a run. A dispatcher may persist a waiting
// state and try again instead of occupying a worker while resources are busy.
func (c *Coordinator) TryCollection(ctx context.Context, sourceID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sourceID == "" || c.barrier == nil || c.sources == nil {
		return nil, invalidResources()
	}
	barrierRelease, err := c.barrier.AcquireMutation(sourceID)
	if err != nil {
		return nil, err
	}
	sourceRelease, err := c.sources.TryAcquireSource(sourceID)
	if err != nil {
		barrierRelease()
		return nil, err
	}
	var once sync.Once
	release := func() { once.Do(func() { sourceRelease(); barrierRelease() }) }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// BackupLease retains the actual media lease. All mount and identity checks
// remain in the media service. Releasing staging after the restic read does not
// release the disk: sync/unmount still require exclusive ownership.
type BackupLease struct {
	ports.DiskLease
	stagingRelease func()
	stagingOnce    sync.Once
	releaseOnce    sync.Once
}

func (l *BackupLease) ReleaseStaging() { l.stagingOnce.Do(l.stagingRelease) }

func (l *BackupLease) Release() {
	l.releaseOnce.Do(func() {
		l.DiskLease.Release()
		l.ReleaseStaging()
	})
}

func (c *Coordinator) TryBackup(ctx context.Context, sourceIDs []string, diskID string) (*BackupLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if diskID == "" || len(sourceIDs) == 0 || c.barrier == nil || c.disks == nil {
		return nil, invalidResources()
	}
	for _, id := range sourceIDs {
		if id == "" {
			return nil, invalidResources()
		}
	}
	release, err := c.barrier.AcquireRead(sourceIDs)
	if err != nil {
		return nil, err
	}
	disk, err := c.disks.Acquire(ctx, diskID)
	if err != nil {
		release()
		return nil, err
	}
	lease := &BackupLease{DiskLease: disk, stagingRelease: release}
	if err := ctx.Err(); err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

// AcquireCollection/AcquireBackup are cancellable waits for callers that already
// own a bounded worker slot. Each unsuccessful attempt releases all resources;
// no waiter holds staging while it waits for a source or disk. Manual operations
// still use their immediate-refusal API. Polling also sees releases made through
// those legacy paths, which do not emit coordinator notifications.
func (c *Coordinator) AcquireCollection(ctx context.Context, sourceID string) (func(), error) {
	for {
		release, err := c.TryCollection(ctx, sourceID)
		if err == nil || !Busy(err) {
			return release, err
		}
		if err := wait(ctx); err != nil {
			return nil, err
		}
	}
}

func (c *Coordinator) AcquireBackup(ctx context.Context, sourceIDs []string, diskID string) (*BackupLease, error) {
	for {
		lease, err := c.TryBackup(ctx, sourceIDs, diskID)
		if err == nil || !Busy(err) {
			return lease, err
		}
		if err := wait(ctx); err != nil {
			return nil, err
		}
	}
}

// Busy identifies only resource contention, never missing media, authentication
// or an arbitrary unavailable error. Those belong to the durable run policy.
func Busy(err error) bool {
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Class != domain.ErrClassUnavailable {
		return false
	}
	switch typed.Code {
	case "source_busy", "disk_busy", "staging_mutation_busy", "staging_read_in_progress", "staging_mutation_in_progress":
		return true
	default:
		return false
	}
}

func wait(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func invalidResources() error {
	return domain.NewError(domain.ErrClassInvariant, "coordinator_resources_invalid",
		"resource coordination requires configured services and nonempty resource identifiers")
}
