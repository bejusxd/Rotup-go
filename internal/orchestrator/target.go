package orchestrator

import (
	"context"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

// TargetStore supplies only durable repository eligibility; no device node is
// stored or passed to the backup service by selection.
type TargetStore interface {
	ListRepositories(context.Context) ([]domain.Repository, error)
	BackupRunsAwaitingRecovery(context.Context, string) ([]domain.BackupRun, error)
}

type TargetMedia interface {
	Reconcile(context.Context) error
	Snapshot(context.Context) (removable.Snapshot, error)
}

type BackupTarget struct {
	RepositoryID string
	DiskID       string
}

// SelectBackupTarget prefers the most recently observed connection among safe,
// available repositories. Unknown times sort behind known ones; ties use stable
// repository IDs, never registration order, device names or last backup time.
// Nil means there is currently no eligible target, not a failed backup attempt.
//
// This is admission, NOT authorization to write. The executor must acquire the
// disk lease, recheck repository health/unfinished writes, and run all existing
// mount UUID and repository identity checks. Selection never mounts or recovers
// media and does not redirect a manual request to a different repository.
func SelectBackupTarget(ctx context.Context, store TargetStore, media TargetMedia) (*BackupTarget, error) {
	// Reconciliation records any observed reconnect before ranking. Its failure
	// must not quietly fall back to stale connection history.
	if err := media.Reconcile(ctx); err != nil {
		return nil, err
	}
	snapshot, err := media.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if !snapshot.PlatformAvailable || snapshot.EnumerationError != "" {
		return nil, domain.NewError(domain.ErrClassUnavailable, "backup_media_observation_unavailable",
			"current backup media could not be safely observed")
	}
	repositories, err := store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var selected *BackupTarget
	var selectedTime time.Time
	for _, repository := range repositories {
		if !repository.Established() || !repository.Health.WritableForBackup() {
			continue
		}
		for _, view := range snapshot.Views {
			if view.Disk == nil || view.Observed == nil || view.Disk.ID != repository.DiskID ||
				!view.Disk.Enabled || view.Busy || view.Ambiguous || !view.Supported ||
				view.Status != domain.DiskAvailableUnmounted || view.ErrorCode != "" ||
				view.Observed.ReadOnly || len(view.ExternalMounts) != 0 {
				continue
			}
			uuid := domain.NormalizeFilesystemUUID(view.Disk.FilesystemUUID)
			if uuid == "" || uuid != domain.NormalizeFilesystemUUID(repository.FilesystemUUID) ||
				uuid != domain.NormalizeFilesystemUUID(view.Observed.FilesystemUUID) {
				continue
			}
			pending, err := store.BackupRunsAwaitingRecovery(ctx, repository.ID)
			if err != nil {
				return nil, err
			}
			if len(pending) != 0 {
				continue
			}
			var connected time.Time
			if view.Disk.LastConnectedAt != nil {
				connected = *view.Disk.LastConnectedAt
			}
			if selected == nil || connected.After(selectedTime) ||
				(connected.Equal(selectedTime) && repository.ID < selected.RepositoryID) {
				selected = &BackupTarget{RepositoryID: repository.ID, DiskID: repository.DiskID}
				selectedTime = connected
			}
		}
	}
	return selected, nil
}
