package backup

import (
	"context"

	"github.com/bejusxd/rotup/internal/coordinator"
	"github.com/bejusxd/rotup/internal/domain"
)

// PrepareJob reserves staging and media for an existing durable intent. It
// performs no mount or restic operation. The dispatcher commits its claim before
// calling work; release is mandatory even when claiming fails.
func (s *Service) PrepareJob(ctx context.Context, run domain.Run, repositoryID string, sourceIDs []string) (func(context.Context) error, func(), error) {
	selected := normalizeSelection(sourceIDs)
	if run.Kind != domain.RunKindBackupLatest || len(selected) == 0 {
		return nil, nil, domain.NewError(domain.ErrClassConfiguration, "backup_sources_empty", "select at least one source to back up")
	}
	repo, disk, err := s.requireRepository(ctx, repositoryID)
	if err != nil {
		return nil, nil, err
	}
	c := coordinator.New(s.barrier, nil, s.media)
	lease, err := c.TryBackup(ctx, selected, disk.ID)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (func(context.Context) error, func(), error) { lease.Release(); return nil, nil, err }
	repo, disk, err = s.requireRepository(ctx, repositoryID)
	if err != nil {
		return fail(err)
	}
	pending, err := s.store.BackupRunsAwaitingRecovery(ctx, repositoryID)
	if err != nil {
		return fail(err)
	}
	previous, err := s.store.BackupRun(ctx, run.ID)
	if err != nil {
		return fail(err)
	}
	if !disk.Enabled || !repo.Established() || !repo.Health.WritableForBackup() || len(pending) != 0 || previous != nil {
		return fail(domain.NewError(domain.ErrClassIntegrity, "repository_not_writable", "the repository or this run has unproven work; explicit recovery is required"))
	}
	for _, id := range selected {
		source, err := s.store.Source(ctx, id)
		if err != nil {
			return fail(err)
		}
		if source == nil {
			return fail(domain.NewError(domain.ErrClassConfiguration, "source_missing", "a selected source no longer exists"))
		}
		if err := eligibleForBackup(*source); err != nil {
			return fail(err)
		}
	}
	target := domain.BackupRun{RunID: run.ID, RepositoryID: repo.ID, DiskID: disk.ID, Semantics: "BACKUP_LATEST", Stage: domain.StageRequested, Durability: domain.DurabilityUnproven, SelectedSourceIDs: selected}
	return func(ctx context.Context) error {
		if err := s.store.CreateBackupRun(ctx, target); err != nil {
			return err
		}
		s.executeBackupPrepared(ctx, run, *repo, target, lease, lease.ReleaseStaging)
		return nil // Dispatcher reads the committed outcome, never infers success.
	}, lease.Release, nil
}
