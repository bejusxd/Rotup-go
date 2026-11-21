package backup

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// StartBackup creates one durable manual BACKUP_LATEST run and executes it in
// the background.
//
// Everything that identifies the target — the repository, the disk, the set of
// sources — is captured here and written once. From this point the run's target
// is a fact in the database, not a query against configuration: a source that is
// disabled, renamed or reconfigured a second later does not change what this run
// was asked to do.
func (s *Service) StartBackup(ctx context.Context, repositoryID string, sourceIDs []string) (string, error) {
	selected := normalizeSelection(sourceIDs)
	if len(selected) == 0 {
		return "", domain.NewError(domain.ErrClassConfiguration, "backup_sources_empty",
			"select at least one source to back up")
	}

	repository, disk, err := s.requireRepository(ctx, repositoryID)
	if err != nil {
		return "", err
	}
	if !disk.Enabled {
		return "", domain.NewError(domain.ErrClassConfiguration, "disk_disabled",
			"this disk is disabled; enable it before backing up to it")
	}
	if !repository.Established() {
		return "", domain.NewError(domain.ErrClassConfiguration, domain.RepositoryErrorNotEstablished,
			"this repository has not been established yet; initialize or adopt it first")
	}
	if !repository.Health.WritableForBackup() {
		return "", domain.NewError(domain.ErrClassIntegrity, "repository_not_writable",
			"this repository is "+string(repository.Health)+
				" and must be recovered before anything writes to it again")
	}

	// Sources are checked for existence here so an obvious mistake is refused
	// immediately. Their eligibility is proven again under the barrier, because
	// only that reading describes the state the backup will actually read.
	for _, sourceID := range selected {
		source, err := s.store.Source(ctx, sourceID)
		if err != nil {
			return "", err
		}
		if source == nil {
			return "", domain.NewError(domain.ErrClassConfiguration, "source_missing",
				"one of the selected sources does not exist")
		}
	}

	// The disk lease is taken before the run row exists, so a request that
	// cannot proceed never leaves an orphaned PENDING run behind, and a
	// duplicate manual request is refused cleanly rather than queued.
	lease, err := s.media.Acquire(ctx, disk.ID)
	if err != nil {
		return "", err
	}
	// Health can have changed since the initial read. Check it and unfinished
	// writes again under the lease, including when failure bookkeeping failed.
	repository, disk, err = s.requireRepository(ctx, repositoryID)
	if err != nil {
		lease.Release()
		return "", err
	}
	pending, err := s.store.BackupRunsAwaitingRecovery(ctx, repositoryID)
	if err != nil {
		lease.Release()
		return "", err
	}
	if !disk.Enabled || !repository.Established() || !repository.Health.WritableForBackup() || len(pending) != 0 {
		lease.Release()
		return "", domain.NewError(domain.ErrClassIntegrity, "repository_not_writable",
			"the repository is disabled or has unfinished work; verify and recover it before backing up")
	}

	run, err := s.newRun(ctx, domain.RunKindBackupLatest, domain.RunTriggerManual, "requested")
	if err != nil {
		lease.Release()
		return "", err
	}
	target := domain.BackupRun{
		RunID:             run.ID,
		RepositoryID:      repository.ID,
		DiskID:            disk.ID,
		Semantics:         "BACKUP_LATEST",
		Stage:             domain.StageRequested,
		Durability:        domain.DurabilityUnproven,
		SelectedSourceIDs: selected,
	}
	if err := s.store.CreateBackupRun(ctx, target); err != nil {
		s.finishRunWithError(ctx, run, err, "the backup request could not be recorded")
		lease.Release()
		return "", err
	}
	s.event(ctx, domain.SeverityInfo, "backup.requested", eventRefs{
		RepositoryID: repository.ID, DiskID: disk.ID, RunID: run.ID,
	}, map[string]any{"sources": len(selected)})

	repositoryCopy := *repository
	s.run(func(background context.Context) {
		defer lease.Release()
		s.executeBackup(background, run, repositoryCopy, target, lease)
	})
	return run.ID, nil
}

// executeBackup performs the manual backup, in the one order that lets every
// step be proven before the next depends on it.
func (s *Service) executeBackup(ctx context.Context, run domain.Run, repository domain.Repository,
	target domain.BackupRun, lease ports.DiskLease) {
	s.executeBackupPrepared(ctx, run, repository, target, lease, nil)
}

func (s *Service) executeBackupPrepared(ctx context.Context, run domain.Run, repository domain.Repository,
	target domain.BackupRun, lease ports.DiskLease, releaseBarrier func()) {

	refs := eventRefs{RepositoryID: repository.ID, DiskID: target.DiskID, RunID: run.ID}
	run = s.startRun(ctx, run, "backing up")

	// The staging barrier is taken before anything is read and before the disk
	// is mounted. Taking it first means the read-set cannot change between
	// being captured and being read, which is the whole reason the barrier
	// exists.
	var err error
	if releaseBarrier == nil {
		releaseBarrier, err = s.barrier.AcquireRead(target.SelectedSourceIDs)
	}
	if err != nil {
		s.failBackup(ctx, run, target, refs, err, domain.StageRequested, false,
			"the staging barrier could not be acquired")
		return
	}
	barrierReleased := false
	releaseBarrierOnce := func() {
		if !barrierReleased {
			barrierReleased = true
			releaseBarrier()
		}
	}
	defer releaseBarrierOnce()

	captureTime := s.now()
	if err := s.store.SetBackupBarrierAcquired(ctx, run.ID, captureTime); err != nil {
		s.failBackup(ctx, run, target, refs, err, domain.StageRequested, false,
			"the staging barrier could not be recorded")
		return
	}

	captured, err := s.captureReadSet(ctx, run.ID, target.SelectedSourceIDs, captureTime)
	if err != nil {
		s.failBackup(ctx, run, target, refs, err, domain.StageRequested, false,
			"the backup read-set could not be captured")
		return
	}
	if err := s.store.SaveReadSet(ctx, run.ID, captured.States, captured.Batches); err != nil {
		s.failBackup(ctx, run, target, refs, err, domain.StageRequested, false,
			"the backup read-set could not be recorded")
		return
	}
	if err := s.store.SetBackupStage(ctx, run.ID, domain.StageReadSetCaptured, captureTime); err != nil {
		s.failBackup(ctx, run, target, refs, err, domain.StageRequested, false,
			"the backup read-set could not be recorded")
		return
	}
	s.event(ctx, domain.SeverityInfo, "backup.read_set_captured", refs, map[string]any{
		"sources":          len(captured.States),
		"revision_batches": len(captured.Batches),
		"paths":            len(captured.Paths),
	})

	s.event(ctx, domain.SeverityInfo, "backup.mount_started", refs, nil)
	if err := lease.Mount(ctx); err != nil {
		// Nothing has been written, so the repository keeps its health.
		s.failBackup(ctx, run, target, refs, err, domain.StageReadSetCaptured, false,
			"the target disk could not be mounted")
		return
	}
	if err := s.store.SetBackupStage(ctx, run.ID, domain.StageMounted, s.now()); err != nil {
		s.unmountAfterFailure(ctx, lease, refs)
		s.failBackup(ctx, run, target, refs, err, domain.StageReadSetCaptured, false,
			"the backup state could not be recorded")
		return
	}
	if err := lease.SetStatus(ctx, domain.DiskBackupRunning); err != nil {
		s.logger.Warn("the disk status could not be set for the backup",
			"disk_id", target.DiskID, "error", err)
	}

	spec, err := s.repositorySpec(ctx, repository, lease)
	if err != nil {
		s.unmountAfterFailure(ctx, lease, refs)
		s.failBackup(ctx, run, target, refs, err, domain.StageMounted, false,
			"the repository location could not be resolved")
		return
	}
	if err := s.verifyRepositoryIdentity(ctx, repository, spec); err != nil {
		// The repository was not what ROTUP recorded, or could not be opened.
		// Nothing has been written, but this is not a state to keep retrying
		// against: it needs a person.
		s.setHealthQuietly(ctx, repository.ID, domain.RepositoryRecoveryRequired, safeMessage(err))
		s.event(ctx, domain.SeverityCritical, "repository.identity_mismatch", refs,
			map[string]any{"reason": safeMessage(err)})
		s.unmountAfterFailure(ctx, lease, refs)
		s.failBackup(ctx, run, target, refs, err, domain.StageMounted, false,
			"the repository identity could not be verified")
		return
	}

	// From here restic may write to the repository, so the stage is recorded
	// first. A crash after this point must be able to tell recovery that a
	// snapshot may exist even though nobody ever saw one.
	resticStarted := s.now()
	if err := s.store.SetBackupStage(ctx, run.ID, domain.StageResticRunning, resticStarted); err != nil {
		s.unmountAfterFailure(ctx, lease, refs)
		s.failBackup(ctx, run, target, refs, err, domain.StageMounted, false,
			"the backup state could not be recorded")
		return
	}
	s.event(ctx, domain.SeverityInfo, "backup.restic_started", refs, map[string]any{
		"paths": len(captured.Paths),
	})

	result, backupErr := s.engine.Backup(ctx, ports.BackupRequest{
		Repository: spec,
		Paths:      captured.Paths,
		Tags: []string{
			domain.TagROTUP,
			domain.RunTag(run.ID),
			domain.RepositoryTag(repository.ID),
			domain.DiskTag(target.DiskID),
			domain.TagBackupLatest,
			domain.TagSnapshotSchema,
		},
	})
	if backupErr != nil {
		s.handleResticFailure(ctx, run, target, repository, refs, lease, backupErr,
			domain.StageResticRunning)
		return
	}
	if signal := result.Stats.FailureSignal(); signal != "" {
		s.handleResticFailure(ctx, run, target, repository, refs, lease,
			domain.NewError(domain.ErrClassIntegrity, "backup_incomplete", signal), domain.StageResticRunning)
		return
	}

	snapshotID, err := domain.ValidateSnapshotID(result.Stats.SnapshotID)
	if err != nil {
		s.handleResticFailure(ctx, run, target, repository, refs, lease, err,
			domain.StageResticRunning)
		return
	}

	counters := domain.BackupRun{
		FilesProcessed:  result.Stats.FilesProcessed,
		FilesNew:        result.Stats.FilesNew,
		FilesChanged:    result.Stats.FilesChanged,
		FilesUnmodified: result.Stats.FilesUnmodified,
		BytesProcessed:  result.Stats.BytesProcessed,
		BytesAdded:      result.Stats.BytesAdded,
		EngineErrors:    result.Stats.Errors,
	}
	if err := s.store.RecordCandidateSnapshot(ctx, run.ID, snapshotID, counters); err != nil {
		s.handleResticFailure(ctx, run, target, repository, refs, lease, err,
			domain.StageResticRunning)
		return
	}
	s.event(ctx, domain.SeverityInfo, "backup.snapshot_created", refs,
		map[string]any{"snapshot_id": snapshotID})

	// The independent read. restic naming a snapshot is restic's own report;
	// finding that snapshot in the repository, carrying this run's tag, is a
	// separate operation reaching a separate conclusion.
	snapshotID, err = s.verifySnapshot(ctx, spec, run.ID, snapshotID, captured.Paths)
	if err != nil {
		s.handleResticFailure(ctx, run, target, repository, refs, lease, err,
			domain.StageSnapshotCreated)
		return
	}
	verifiedAt := s.now()
	if err := s.store.RecordVerifiedSnapshot(ctx, run.ID, snapshotID, verifiedAt); err != nil {
		s.handleResticFailure(ctx, run, target, repository, refs, lease, err,
			domain.StageSnapshotCreated)
		return
	}
	s.event(ctx, domain.SeverityInfo, "backup.snapshot_verified", refs,
		map[string]any{"snapshot_id": snapshotID})

	// restic has finished reading staging, so the barrier is released before
	// the flush and unmount. Holding it through media work would stall
	// collection for no benefit: nothing after this reads staging.
	releaseBarrierOnce()

	s.event(ctx, domain.SeverityInfo, "backup.sync_started", refs, nil)
	if err := lease.SyncAndUnmount(ctx); err != nil {
		s.handleDurabilityFailure(ctx, run, target, repository, refs, snapshotID, err)
		return
	}
	durableAt := s.now()
	// The flush and the unmount are a single fail-closed step in the media
	// service, so both stages are recorded once it reports success. A failure
	// distinguishes them through the disk error code it records.
	if err := s.store.SetBackupStage(ctx, run.ID, domain.StageSynced, durableAt); err != nil {
		s.handleDurabilityFailure(ctx, run, target, repository, refs, snapshotID, err)
		return
	}
	if err := s.store.SetBackupStage(ctx, run.ID, domain.StageUnmountVerified, durableAt); err != nil {
		s.handleDurabilityFailure(ctx, run, target, repository, refs, snapshotID, err)
		return
	}
	s.event(ctx, domain.SeverityInfo, "backup.unmount_verified", refs, nil)

	s.completeBackup(ctx, run, target, repository, refs, snapshotID, counters,
		captured.States, captured.Batches, domain.DurabilityNormal, durableAt, result.Warnings)
}

// verifySnapshot confirms, by a separate read of the repository, that the
// snapshot restic named exists and belongs to this run.
//
// The tag check matters as much as the identifier. A snapshot with the right ID
// but without this run's tag would mean ROTUP is looking at something it did not
// create, and treating that as proof of its own backup would be exactly the kind
// of false success this milestone exists to prevent.
func (s *Service) verifySnapshot(ctx context.Context, spec ports.RepositorySpec,
	runID, snapshotID string, paths []string) (string, error) {

	snapshots, err := s.engine.Snapshots(ctx, spec)
	if err != nil {
		return "", err
	}
	found := findSnapshot(snapshots, snapshotID, domain.RunTag(runID))
	if found == nil {
		return "", domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorSnapshotAbsent,
			"the snapshot restic reported could not be found in the repository, so the backup is not proven")
	}
	if len(found.Paths) != len(paths) || !snapshotContainsPaths(*found, paths) {
		return "", domain.NewError(domain.ErrClassIntegrity, "backup_paths_incomplete",
			"the snapshot does not contain the complete requested staging path set")
	}
	return domain.ValidateSnapshotID(found.ID)
}

func snapshotContainsPaths(snapshot ports.Snapshot, paths []string) bool {
	normalize := func(path string) string {
		path = filepath.Clean(filepath.FromSlash(path))
		if runtime.GOOS == "windows" {
			path = strings.ToLower(path)
		}
		return path
	}
	actual := make(map[string]bool, len(snapshot.Paths))
	for _, path := range snapshot.Paths {
		actual[normalize(path)] = true
	}
	for _, path := range paths {
		if !actual[normalize(path)] {
			return false
		}
	}
	return true
}

// findSnapshot locates a snapshot by identifier and required tag. restic reports
// both a full identifier and its conventional short form, and a caller may hold
// either, so both are matched.
func findSnapshot(snapshots []ports.Snapshot, snapshotID, requiredTag string) *ports.Snapshot {
	wanted := strings.ToLower(strings.TrimSpace(snapshotID))
	if wanted == "" {
		return nil
	}
	var found *ports.Snapshot
	for i := range snapshots {
		candidate := snapshots[i]
		matches := candidate.ID == wanted || candidate.ShortID == wanted ||
			(len(candidate.ID) > len(wanted) && strings.HasPrefix(candidate.ID, wanted))
		if !matches {
			continue
		}
		if requiredTag != "" && !candidate.HasTag(requiredTag) {
			continue
		}
		if found != nil {
			return nil // An ambiguous short ID is not proof.
		}
		found = &snapshots[i]
	}
	return found
}

// snapshotForRun locates this run's snapshot by its tag alone. Recovery uses it
// when no candidate identifier was ever recorded, which is what an interruption
// during restic itself leaves behind.
func snapshotForRun(snapshots []ports.Snapshot, runID string) *ports.Snapshot {
	tag := domain.RunTag(runID)
	var found *ports.Snapshot
	for i := range snapshots {
		if snapshots[i].HasTag(tag) {
			if found != nil {
				return nil
			}
			found = &snapshots[i]
		}
	}
	return found
}

// completeBackup writes the durable success. Every fact that may only be true of
// a proven backup is written in the store's single completion transaction.
func (s *Service) completeBackup(ctx context.Context, run domain.Run, target domain.BackupRun,
	repository domain.Repository, refs eventRefs, snapshotID string, counters domain.BackupRun,
	states []domain.BackupSourceState, batches []domain.BackupRevisionBatch,
	durability domain.BackupDurability, at time.Time, warnings []string) error {

	coverage := make([]domain.RevisionCoverage, 0, len(batches))
	for _, batch := range batches {
		coverage = append(coverage, domain.RevisionCoverage{
			RevisionBatchID: batch.RevisionBatchID,
			BackupRunID:     run.ID,
			RepositoryID:    repository.ID,
			DiskID:          target.DiskID,
			SnapshotID:      snapshotID,
			CoveredAt:       at,
		})
	}

	summary := backupSummary{
		RepositoryID:    repository.ID,
		DiskID:          target.DiskID,
		SnapshotID:      snapshotID,
		Sources:         len(states),
		RevisionBatches: len(batches),
		FilesProcessed:  counters.FilesProcessed,
		BytesProcessed:  counters.BytesProcessed,
		BytesAdded:      counters.BytesAdded,
		Durability:      string(durability),
	}

	run.Status = domain.RunStatusSucceeded
	if len(warnings) > 0 || durability == domain.DurabilityAdopted {
		run.Status = domain.RunStatusSucceededWithWarning
	}
	run.FinishedAt = at
	run.Summary = describeCounts(snapshotID, summary)
	if durability == domain.DurabilityAdopted {
		run.Summary = "recovered: " + run.Summary +
			" — this backup was interrupted and its snapshot was confirmed by a later repository recovery"
	}
	run.SummaryJSON = encodeSummary(summary)

	completed := target
	completed.VerifiedSnapshotID = snapshotID
	completed.Durability = durability
	completed.DurableSuccessAt = at
	completed.SyncedAt = at
	completed.UnmountVerifiedAt = at

	if err := s.store.CompleteBackupRun(ctx, run, completed, coverage); err != nil {
		// The bytes are on the media, but ROTUP could not record that fact.
		// Claiming success it cannot read back after a restart would be worse
		// than reporting the failure, so the repository becomes suspect and a
		// recovery can adopt the snapshot later.
		s.markSuspect(ctx, repository.ID,
			"a verified backup could not be recorded, so its result must be confirmed by recovery")
		s.finishRunWithError(ctx, run, err, "the backup result could not be recorded")
		return err
	}

	eventType := "backup.succeeded"
	if durability == domain.DurabilityAdopted {
		eventType = "backup.recovered_success"
	}
	s.event(ctx, domain.SeverityInfo, eventType, refs, map[string]any{
		"snapshot_id":      snapshotID,
		"sources":          len(states),
		"revision_batches": len(batches),
		"durability":       string(durability),
	})
	for _, item := range coverage {
		s.event(ctx, domain.SeverityInfo, "revision.covered", eventRefs{
			RepositoryID: repository.ID, DiskID: target.DiskID, RunID: run.ID,
		}, map[string]any{
			"revision_batch_id": item.RevisionBatchID,
			"snapshot_id":       snapshotID,
		})
	}
	s.logger.Info("backup succeeded",
		"run_id", run.ID, "repository_id", repository.ID, "disk_id", target.DiskID,
		"snapshot_id", snapshotID, "durability", string(durability))
	return nil
}

// handleResticFailure records a failure that happened once restic had been
// started, so the repository may have been written to.
func (s *Service) handleResticFailure(ctx context.Context, run domain.Run, target domain.BackupRun,
	repository domain.Repository, refs eventRefs, lease ports.DiskLease, cause error,
	stage domain.DurabilityStage) {

	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	removed := s.mediaGone(ctx, lease)
	reason := safeMessage(cause)
	if removed {
		reason = "the disk was disconnected during the backup, so the repository state is unknown"
	}
	s.markSuspect(ctx, repository.ID, reason)

	if !removed {
		// Only attempt an unmount when the media is still there. Trying to flush
		// a device that is gone produces a second, less informative error.
		s.unmountAfterFailure(ctx, lease, refs)
	}

	failure := cause
	if removed {
		failure = domain.WrapError(domain.ErrClassIntegrity, domain.DiskErrorRemovedDuringOperation,
			"the disk was disconnected during the backup, so ROTUP cannot say what reached the media", cause)
	}
	s.failBackup(ctx, run, target, refs, failure, stage, true, "the backup failed")
}

// handleDurabilityFailure records the case where a snapshot was verified but the
// flush or the unmount did not complete.
//
// This is the situation the milestone's success rule exists for. The snapshot is
// real and was confirmed, but nothing establishes that the bytes reached the
// media, and reporting success on the strength of a verified snapshot alone
// would be a claim about durability that was never tested.
func (s *Service) handleDurabilityFailure(ctx context.Context, run domain.Run, target domain.BackupRun,
	repository domain.Repository, refs eventRefs, snapshotID string, cause error) {

	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	stage := domain.StageSnapshotVerified
	var typed *domain.Error
	if domain.AsError(cause, &typed) && typed.Code == domain.DiskErrorUnmountFailed {
		// The flush completed and the unmount did not, which is a different
		// fact and worth keeping.
		stage = domain.StageSynced
	}

	s.markSuspect(ctx, repository.ID,
		"a snapshot was verified but the disk could not be flushed and unmounted, "+
			"so ROTUP cannot confirm the backup reached the media")
	s.event(ctx, domain.SeverityError, "backup.durability_unproven", refs, map[string]any{
		"snapshot_id": snapshotID,
		"reason":      safeMessage(cause),
	})
	s.failBackup(ctx, run, target, refs, cause, stage, true,
		"the backup could not be proven durable")
}

// failBackup writes a terminal failure onto both the run and its backup detail.
func (s *Service) failBackup(ctx context.Context, run domain.Run, target domain.BackupRun,
	refs eventRefs, cause error, stage domain.DurabilityStage, repositoryTouched bool,
	fallback string) {

	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if repositoryTouched {
		// The stage is preserved rather than overwritten with FAILED, because
		// it is what tells a later recovery how far this run got and whether a
		// snapshot may exist.
		if err := s.store.SetBackupStage(ctx, run.ID, stage, s.now()); err != nil {
			s.logger.Error("the backup stage could not be recorded", "run_id", run.ID, "error", err)
		}
	} else if err := s.store.FailBackupRun(ctx, run.ID, domain.StageFailed); err != nil {
		s.logger.Error("the backup failure could not be recorded", "run_id", run.ID, "error", err)
	}

	finished := s.finishRunWithError(ctx, run, cause, fallback)
	eventType := "backup.failed"
	severity := domain.SeverityError
	if finished.Status == domain.RunStatusInterrupted {
		eventType = "backup.interrupted"
	}
	s.event(ctx, severity, eventType, refs, map[string]any{
		"stage":  string(stage),
		"reason": safeMessage(cause),
	})
	s.logger.Warn("backup did not succeed",
		"run_id", run.ID, "repository_id", target.RepositoryID, "disk_id", target.DiskID,
		"stage", string(stage), "status", string(finished.Status))
}

// mediaGone reports whether the leased disk has disappeared. An error reading
// the system is treated as "cannot say", not as "still there".
func (s *Service) mediaGone(ctx context.Context, lease ports.DiskLease) bool {
	present, err := lease.Present(ctx)
	if err != nil {
		return false
	}
	return !present
}

// unmountAfterFailure makes a best effort to leave the media unmounted. A
// failure here is logged and recorded by the media service; it never converts
// the original failure into something else.
func (s *Service) unmountAfterFailure(ctx context.Context, lease ports.DiskLease, refs eventRefs) {
	if !lease.Mounted() {
		return
	}
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := lease.SyncAndUnmount(ctx); err != nil {
		s.logger.Error("the disk could not be flushed and unmounted after a failure",
			"disk_id", lease.DiskID(), "error", err)
		s.event(ctx, domain.SeverityError, "backup.unmount_failed", refs,
			map[string]any{"reason": safeMessage(err)})
	}
}

// normalizeSelection removes blanks and duplicates while keeping the
// administrator's order, so the recorded target is exactly what was chosen.
func normalizeSelection(sourceIDs []string) []string {
	seen := make(map[string]struct{}, len(sourceIDs))
	result := make([]string, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		trimmed := strings.TrimSpace(sourceID)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
