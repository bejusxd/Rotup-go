package backup

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// RecoveryReport summarizes what startup recovery changed.
type RecoveryReport struct {
	InterruptedRuns       int
	RepositoriesSuspected int
}

// Recover is the startup crash boundary for repository work. It runs before the
// daemon accepts any new request.
//
// Its rule is that a run a previous process left active is never resolved
// optimistically. A stale RUNNING row says the daemon died while the operation
// was in flight, and nothing about that says whether the operation succeeded. So
// the run becomes INTERRUPTED, and if restic had been started the repository
// becomes SUSPECT: a snapshot may exist that nobody confirmed, and until an
// explicit recovery looks, ROTUP will not write to that repository again.
func (s *Service) Recover(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport

	active, err := s.store.ActiveRepositoryRuns(ctx)
	if err != nil {
		return report, err
	}
	for _, run := range active {
		if err := s.recoverRun(ctx, run, &report); err != nil {
			return report, err
		}
	}

	// A repository left mid-establishment by a crash is suspect on its own
	// account: restic may have created it, and nothing proved it afterwards.
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return report, err
	}
	for _, repository := range repositories {
		pending, err := s.store.BackupRunsAwaitingRecovery(ctx, repository.ID)
		if err != nil {
			return report, err
		}
		// Also sweep terminal runs: a crash can happen after their status was
		// saved but before the repository was marked suspect.
		if repository.Health != domain.RepositoryInitializing &&
			!(repository.Health.WritableForBackup() && len(pending) > 0) {
			continue
		}
		if err := s.markSuspect(ctx, repository.ID,
			"an unfinished repository operation requires explicit recovery"); err != nil {
			return report, err
		}
		report.RepositoriesSuspected++
	}
	return report, nil
}

func (s *Service) recoverRun(ctx context.Context, run domain.Run, report *RecoveryReport) error {
	run.Status = domain.RunStatusInterrupted
	run.FinishedAt = s.now()
	run.ErrorClass = domain.ErrClassCancellation
	run.ErrorCode = domain.RepositoryErrorInterrupted
	run.SafeError = "the daemon stopped while this operation was running"
	run.Summary = run.SafeError
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return err
	}
	report.InterruptedRuns++

	backup, err := s.store.BackupRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if backup == nil {
		// A DISK_SETUP or VERIFY run. The repository sweep above handles a
		// setup that was left mid-flight; a verification writes nothing, so
		// there is nothing further to undo.
		s.event(ctx, domain.SeverityWarning, "repository.recovery_started", eventRefs{RunID: run.ID},
			map[string]any{"reason": "the daemon stopped while a repository operation was running"})
		return nil
	}

	refs := eventRefs{RepositoryID: backup.RepositoryID, DiskID: backup.DiskID, RunID: run.ID}
	if !backup.Stage.RepositoryMayHaveChanged() {
		// restic was never started, so the repository is exactly as it was.
		if err := s.store.FailBackupRun(ctx, run.ID, domain.StageFailed); err != nil {
			return err
		}
		s.event(ctx, domain.SeverityWarning, "backup.interrupted", refs,
			map[string]any{"stage": string(backup.Stage), "repository_touched": false})
		return nil
	}

	reason := "a backup was interrupted after restic had started, so the repository may hold a snapshot nobody confirmed"
	if backup.CandidateSnapshotID != "" {
		reason = "a backup was interrupted after restic reported snapshot " +
			shortID(backup.CandidateSnapshotID) + ", which has not been confirmed"
	}
	if err := s.markSuspect(ctx, backup.RepositoryID, reason); err != nil {
		return err
	}
	report.RepositoriesSuspected++
	s.event(ctx, domain.SeverityWarning, "backup.interrupted", refs, map[string]any{
		"stage":                 string(backup.Stage),
		"repository_touched":    true,
		"candidate_snapshot_id": backup.CandidateSnapshotID,
	})
	return nil
}

// StartRepositoryRecovery creates an explicit VERIFY run that proves a
// repository again and resolves any backup an interruption left unfinished.
//
// Recovery is always a deliberate administrator action. ROTUP does not quietly
// re-run a backup against a reinserted disk, and it does not repair anything: it
// reads, it proves, and it records what it found.
func (s *Service) StartRepositoryRecovery(ctx context.Context, repositoryID string) (string, error) {
	repository, disk, err := s.requireRepository(ctx, repositoryID)
	if err != nil {
		return "", err
	}
	if !repository.Established() {
		return "", domain.NewError(domain.ErrClassConfiguration, domain.RepositoryErrorNotEstablished,
			"this repository has never been established, so there is nothing to recover")
	}
	if !disk.Enabled {
		return "", domain.NewError(domain.ErrClassConfiguration, "disk_disabled",
			"this disk is disabled; enable it before recovering its repository")
	}

	lease, err := s.media.Acquire(ctx, disk.ID)
	if err != nil {
		return "", err
	}
	run, err := s.newRun(ctx, domain.RunKindVerify, domain.RunTriggerRecovery, "requested")
	if err != nil {
		lease.Release()
		return "", err
	}
	s.event(ctx, domain.SeverityInfo, "repository.recovery_started", eventRefs{
		RepositoryID: repository.ID, DiskID: disk.ID, RunID: run.ID,
	}, nil)

	target := *repository
	s.run(func(background context.Context) {
		defer lease.Release()
		s.executeRecovery(background, run, target, lease)
	})
	return run.ID, nil
}

// executeRecovery mounts the disk, proves the repository, resolves interrupted
// backups, and returns the repository to HEALTHY only if every proof succeeded.
func (s *Service) executeRecovery(ctx context.Context, run domain.Run, repository domain.Repository,
	lease ports.DiskLease) {

	refs := eventRefs{RepositoryID: repository.ID, DiskID: repository.DiskID, RunID: run.ID}
	run = s.startRun(ctx, run, "verifying the repository")
	if err := s.markSuspect(ctx, repository.ID, "explicit repository verification is in progress"); err != nil {
		s.finishRunWithError(ctx, run, err, "the repository verification state could not be recorded")
		return
	}

	if err := lease.Mount(ctx); err != nil {
		s.finishRunWithError(ctx, run, err, "the disk could not be mounted")
		return
	}
	if err := lease.SetStatus(ctx, domain.DiskVerifying); err != nil {
		s.logger.Warn("the disk status could not be set for the recovery",
			"disk_id", repository.DiskID, "error", err)
	}

	outcome, verifyErr := s.proveRepository(ctx, repository, lease)

	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	unmountErr := lease.SyncAndUnmount(ctx)

	switch {
	case verifyErr != nil:
		// The repository could not be proven. RECOVERY_REQUIRED rather than
		// SUSPECT: a recovery has already looked and could not establish that
		// the repository is usable, so the next step is a person, not another
		// automatic attempt.
		s.setHealthQuietly(ctx, repository.ID, domain.RepositoryRecoveryRequired, safeMessage(verifyErr))
		s.event(ctx, domain.SeverityError, "repository.recovery_failed", refs,
			map[string]any{"reason": safeMessage(verifyErr)})
		s.finishRunWithError(ctx, run, verifyErr, "the repository could not be verified")
		return

	case unmountErr != nil:
		s.setHealthQuietly(ctx, repository.ID, domain.RepositorySuspect,
			"the repository was verified but the disk could not be flushed and unmounted afterwards")
		s.finishRunWithError(ctx, run, unmountErr,
			"the disk could not be flushed and unmounted after verification")
		return
	}

	// No adoption or coverage may be committed while the repository is still
	// mounted, even when its snapshot listing and structural check succeeded.
	adopted, abandoned, err := s.resolveInterruptedBackups(ctx, repository, outcome.Snapshots)
	if err != nil {
		s.markSuspect(ctx, repository.ID, "recovery results could not be recorded")
		s.finishRunWithError(ctx, run, err, "recovery results could not be recorded")
		return
	}
	outcome.Adopted, outcome.Abandoned = adopted, abandoned

	if err := s.store.SetRepositoryHealth(ctx, repository.ID, domain.RepositoryHealthy, ""); err != nil {
		s.finishRunWithError(ctx, run, err, "the repository state could not be recorded")
		return
	}
	s.event(ctx, domain.SeverityInfo, "repository.recovered", refs, map[string]any{
		"snapshots":         outcome.SnapshotCount,
		"backups_adopted":   outcome.Adopted,
		"backups_abandoned": outcome.Abandoned,
	})

	run.Status = domain.RunStatusSucceeded
	run.FinishedAt = s.now()
	run.Summary = fmt.Sprintf("repository verified: %d snapshot(s), %d interrupted backup(s) adopted, %d abandoned",
		outcome.SnapshotCount, outcome.Adopted, outcome.Abandoned)
	run.SummaryJSON = encodeSummary(map[string]any{
		"repository_id":     repository.ID,
		"disk_id":           repository.DiskID,
		"snapshots":         outcome.SnapshotCount,
		"backups_adopted":   outcome.Adopted,
		"backups_abandoned": outcome.Abandoned,
		"check_errors":      outcome.CheckErrors,
	})
	s.saveRun(ctx, run)
}

// recoveryOutcome is what one recovery established.
type recoveryOutcome struct {
	Snapshots     []ports.Snapshot
	SnapshotCount int
	Adopted       int
	Abandoned     int
	CheckErrors   int64
}

// proveRepository runs the M4 verification set. Every step is non-destructive.
//
// The sequence is deliberately ordered from cheapest and most fundamental to
// most expensive: the repository must be openable and its identity must match
// before anything else is worth doing, its snapshot listing must be readable
// before any snapshot can be searched for, and only then is a structural check
// worth its time.
//
// What this does not do is read every pack file. restic's check verifies
// repository structure, indexes and metadata; verifying the data itself is
// --read-data, which is proportional to repository size and belongs with the
// scheduled verification work in M8. A recovery here therefore establishes that
// the repository is structurally sound and that a specific snapshot exists; it
// does not certify every byte of history.
func (s *Service) proveRepository(ctx context.Context, repository domain.Repository,
	lease ports.DiskLease) (recoveryOutcome, error) {

	var outcome recoveryOutcome

	spec, err := s.repositorySpec(ctx, repository, lease)
	if err != nil {
		return outcome, err
	}
	if err := s.verifyRepositoryIdentity(ctx, repository, spec); err != nil {
		return outcome, err
	}

	snapshots, err := s.engine.Snapshots(ctx, spec)
	if err != nil {
		return outcome, err
	}
	outcome.SnapshotCount = len(snapshots)
	outcome.Snapshots = snapshots

	check, err := s.engine.Check(ctx, spec)
	outcome.CheckErrors = check.Errors
	if err != nil {
		return outcome, err
	}
	if check.Errors != 0 {
		return outcome, domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorUnreadable,
			"the repository check reported errors")
	}
	return outcome, nil
}

// resolveInterruptedBackups decides, for every backup this repository left
// unfinished, whether its snapshot actually exists.
//
// This is the ambiguous-success case the architecture calls out: restic created
// a snapshot, then the daemon or the media went away before ROTUP could prove
// the result. Finding that snapshot alongside a recorded clean engine result
// and matching read-set permits a distinct success. It has adopted durability
// and its own event, because it did not travel the normal path and history
// should not pretend it did.
func (s *Service) resolveInterruptedBackups(ctx context.Context, repository domain.Repository,
	snapshots []ports.Snapshot) (int, int, error) {

	pending, err := s.store.BackupRunsAwaitingRecovery(ctx, repository.ID)
	if err != nil {
		return 0, 0, err
	}

	adopted, abandoned := 0, 0
	for _, backup := range pending {
		refs := eventRefs{RepositoryID: repository.ID, DiskID: backup.DiskID, RunID: backup.RunID}

		// Locate by run tag, but never infer a clean backup from existence:
		// restic also saves incomplete snapshots (exit 3). Its snapshot format
		// has no completion/error flag. A durably recorded candidate is our
		// evidence that the engine exited cleanly; unknown exits earn no coverage.
		found := snapshotForRun(snapshots, backup.RunID)
		if found == nil {
			ambiguous := false
			for _, snapshot := range snapshots {
				ambiguous = ambiguous || snapshot.HasTag(domain.RunTag(backup.RunID))
			}
			if ambiguous {
				if err := s.declineBackup(ctx, backup.RunID, refs,
					"multiple snapshots carry this run's tag; recovery cannot attribute a unique result"); err != nil {
					return adopted, abandoned, err
				}
				abandoned++
				continue
			}
			if err := s.abandonRun(ctx, backup.RunID, refs); err != nil {
				return adopted, abandoned, err
			}
			if err := s.store.FailBackupRun(ctx, backup.RunID, domain.StageFailed); err != nil {
				return adopted, abandoned, err
			}
			abandoned++
			continue
		}
		if backup.CandidateSnapshotID == "" || backup.EngineErrors != 0 ||
			findSnapshot([]ports.Snapshot{*found}, backup.CandidateSnapshotID, "") == nil {
			if err := s.declineBackup(ctx, backup.RunID, refs,
				"a snapshot exists, but this run has no matching recorded clean restic completion; no coverage was granted"); err != nil {
				return adopted, abandoned, err
			}
			abandoned++
			continue
		}

		snapshotID, err := domain.ValidateSnapshotID(found.ID)
		if err != nil {
			return adopted, abandoned, err
		}
		ok, err := s.adoptBackup(ctx, repository, backup, *found, snapshotID, refs)
		if err != nil {
			return adopted, abandoned, err
		}
		if ok {
			adopted++
		} else {
			abandoned++
		}
	}
	return adopted, abandoned, nil
}

// adoptBackup turns a proven interrupted backup into an adopted success,
// covering exactly the read-set it recorded before it started.
func (s *Service) adoptBackup(ctx context.Context, repository domain.Repository,
	backup domain.BackupRun, snapshot ports.Snapshot, snapshotID string, refs eventRefs) (bool, error) {

	run, err := s.store.Run(ctx, backup.RunID)
	if err != nil {
		return false, err
	}
	if run == nil {
		return false, domain.NewError(domain.ErrClassInvariant, "run_missing",
			"a backup run being recovered no longer exists")
	}

	states, err := s.store.BackupSourceStates(ctx, backup.RunID)
	if err != nil {
		return false, err
	}
	batches, err := s.store.BackupRevisionBatches(ctx, backup.RunID)
	if err != nil {
		return false, err
	}
	if len(states) == 0 {
		// A run with no recorded read-set cannot be adopted: nothing says what
		// the snapshot contains, so no coverage could honestly be derived from
		// it. The snapshot stays in the repository and the run stays failed.
		return false, s.declineBackup(ctx, backup.RunID, refs,
			"the interrupted run has no recorded read-set, so its snapshot cannot be attributed")
	}
	paths := make([]string, 0, len(states)+len(batches))
	for _, state := range states {
		paths = append(paths, filepath.Join(s.layout.Root(), filepath.FromSlash(state.CurrentRelativePath)))
	}
	for _, batch := range batches {
		paths = append(paths, filepath.Join(s.layout.Root(), filepath.FromSlash(batch.RelativePath)))
	}
	if len(states) != len(backup.SelectedSourceIDs) || !snapshotContainsPaths(snapshot, paths) {
		return false, s.declineBackup(ctx, backup.RunID, refs,
			"the snapshot does not contain the recorded staging read-set; no coverage was granted")
	}

	err = s.completeBackup(ctx, *run, backup, repository, refs, snapshotID, backup,
		states, batches, domain.DurabilityAdopted, s.now(), nil)
	return err == nil, err
}

func (s *Service) declineBackup(ctx context.Context, runID string, refs eventRefs, reason string) error {
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return domain.NewError(domain.ErrClassInvariant, "run_missing", "the interrupted backup run is missing")
	}
	run.Status = domain.RunStatusFailed
	run.FinishedAt = s.now()
	run.ErrorClass = domain.ErrClassIntegrity
	run.ErrorCode = "backup_recovery_declined"
	run.SafeError, run.Summary = reason, reason
	if err := s.store.UpdateRun(ctx, *run); err != nil {
		return err
	}
	if err := s.store.FailBackupRun(ctx, runID, domain.StageFailed); err != nil {
		return err
	}
	s.event(ctx, domain.SeverityWarning, "backup.recovery_declined", refs, map[string]any{"reason": reason})
	return nil
}

// abandonRun records that an interrupted backup produced no snapshot.
func (s *Service) abandonRun(ctx context.Context, runID string, refs eventRefs) error {
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return nil
	}
	if run.Status.Successful() {
		// A run that already reached a proven success is not revisited.
		return nil
	}
	run.Status = domain.RunStatusFailed
	if run.FinishedAt.IsZero() {
		run.FinishedAt = s.now()
	}
	run.ErrorClass = domain.ErrClassIntegrity
	run.ErrorCode = domain.RepositoryErrorSnapshotAbsent
	run.SafeError = "this backup was interrupted and recovery found no snapshot for it in the repository"
	run.Summary = run.SafeError
	if err := s.store.UpdateRun(ctx, *run); err != nil {
		return err
	}
	s.event(ctx, domain.SeverityWarning, "backup.recovery_found_no_snapshot", refs, nil)
	return nil
}
