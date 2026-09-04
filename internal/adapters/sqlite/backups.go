package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const backupRunColumns = `run_id, repository_id, disk_id, semantics, durability_stage,
	durability, selected_source_ids_json, read_set_captured_at, barrier_acquired_at,
	restic_started_at, candidate_snapshot_id, verified_snapshot_id, snapshot_verified_at,
	synced_at, unmount_verified_at, durable_success_at, files_processed, files_new,
	files_changed, files_unmodified, bytes_processed, bytes_added, engine_errors`

// CreateBackupRun records the immutable target of one manual backup request.
//
// The target is written once, at request time, and is never recomputed. A
// configuration change made after the request must not be able to redirect the
// backup at a different disk or a different set of sources: what the
// administrator asked for is what history has to show.
func (s *Store) CreateBackupRun(ctx context.Context, backup domain.BackupRun) error {
	selected, err := json.Marshal(nonNilStrings(backup.SelectedSourceIDs))
	if err != nil {
		return fmt.Errorf("encode selected sources: %w", err)
	}
	if backup.Semantics == "" {
		backup.Semantics = "BACKUP_LATEST"
	}
	if backup.Durability == "" {
		backup.Durability = domain.DurabilityUnproven
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO backup_runs (run_id, repository_id, disk_id, semantics,
				durability_stage, durability, selected_source_ids_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			backup.RunID,
			backup.RepositoryID,
			backup.DiskID,
			backup.Semantics,
			string(backup.Stage),
			string(backup.Durability),
			string(selected),
		); err != nil {
			return fmt.Errorf("create backup run: %w", err)
		}
		return nil
	})
}

// SetBackupStage advances a backup run's durability stage.
//
// Each stage carries the fact that justifies it, written in the same statement,
// so a stage can never claim more than what was recorded alongside it. The
// timestamp columns use COALESCE so that re-entering a stage during recovery
// does not rewrite when it first happened.
func (s *Store) SetBackupStage(ctx context.Context, runID string, stage domain.DurabilityStage,
	at time.Time) error {

	timestamp := formatTime(at)
	var column string
	switch stage {
	case domain.StageReadSetCaptured:
		column = "read_set_captured_at"
	case domain.StageResticRunning:
		column = "restic_started_at"
	case domain.StageSnapshotVerified:
		column = "snapshot_verified_at"
	case domain.StageSynced:
		column = "synced_at"
	case domain.StageUnmountVerified:
		column = "unmount_verified_at"
	}

	statement := `UPDATE backup_runs SET durability_stage = ? WHERE run_id = ?`
	arguments := []any{string(stage), runID}
	if column != "" {
		statement = `UPDATE backup_runs SET durability_stage = ?, ` + column +
			` = COALESCE(` + column + `, ?) WHERE run_id = ?`
		arguments = []any{string(stage), timestamp, runID}
	}

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, statement, arguments...)
		if err != nil {
			return fmt.Errorf("advance backup stage: %w", err)
		}
		return requireBackupRunRow(result)
	})
}

// SetBackupBarrierAcquired records when the staging read barrier was taken.
func (s *Store) SetBackupBarrierAcquired(ctx context.Context, runID string, at time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE backup_runs SET barrier_acquired_at = COALESCE(barrier_acquired_at, ?)
			WHERE run_id = ?`, formatTime(at), runID)
		if err != nil {
			return fmt.Errorf("record staging barrier: %w", err)
		}
		return requireBackupRunRow(result)
	})
}

// RecordCandidateSnapshot stores the snapshot identifier restic named, together
// with its counters, and moves the run to SNAPSHOT_CREATED.
//
// This is written before the snapshot has been independently verified, and that
// is the point. If the daemon or the media disappears between restic finishing
// and ROTUP proving the result, this row is the only thing that tells a later
// recovery which snapshot to look for.
func (s *Store) RecordCandidateSnapshot(ctx context.Context, runID, snapshotID string,
	stats domain.BackupRun) error {

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE backup_runs
			SET durability_stage = ?, candidate_snapshot_id = ?,
			    files_processed = ?, files_new = ?, files_changed = ?,
			    files_unmodified = ?, bytes_processed = ?, bytes_added = ?,
			    engine_errors = ?
			WHERE run_id = ?`,
			string(domain.StageSnapshotCreated),
			snapshotID,
			stats.FilesProcessed,
			stats.FilesNew,
			stats.FilesChanged,
			stats.FilesUnmodified,
			stats.BytesProcessed,
			stats.BytesAdded,
			stats.EngineErrors,
			runID,
		)
		if err != nil {
			return fmt.Errorf("record candidate snapshot: %w", err)
		}
		return requireBackupRunRow(result)
	})
}

// RecordVerifiedSnapshot stores the snapshot identifier an independent read
// confirmed, and moves the run to SNAPSHOT_VERIFIED.
func (s *Store) RecordVerifiedSnapshot(ctx context.Context, runID, snapshotID string, at time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE backup_runs
			SET durability_stage = ?, verified_snapshot_id = ?,
			    snapshot_verified_at = COALESCE(snapshot_verified_at, ?)
			WHERE run_id = ?`,
			string(domain.StageSnapshotVerified), snapshotID, formatTime(at), runID)
		if err != nil {
			return fmt.Errorf("record verified snapshot: %w", err)
		}
		return requireBackupRunRow(result)
	})
}

// CompleteBackupRun is the single transaction that turns a proven backup into a
// durable success.
//
// Everything that may only be true of a durable success is written here and
// nowhere else: the run's terminal status, the run's durability stage, the
// repository's last snapshot, and the revision coverage rows. Splitting them
// would create a window in which one of them is true and the others are not,
// and every one of those windows is a way for ROTUP to claim more than it has
// proven.
func (s *Store) CompleteBackupRun(ctx context.Context, run domain.Run, backup domain.BackupRun,
	coverage []domain.RevisionCoverage) error {

	if backup.VerifiedSnapshotID == "" {
		return domain.NewError(domain.ErrClassInvariant, "backup_snapshot_missing",
			"a durable backup success must name the snapshot it verified")
	}
	if run.ID != backup.RunID || !run.Status.Successful() ||
		backup.SyncedAt.IsZero() || backup.UnmountVerifiedAt.IsZero() || backup.DurableSuccessAt.IsZero() {
		return domain.NewError(domain.ErrClassInvariant, "backup_durability_unproven",
			"backup completion requires a matching successful run and verified sync/unmount")
	}
	if run.SummaryJSON == "" {
		run.SummaryJSON = "{}"
	}
	run.UpdatedAt = time.Now().UTC()

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE backup_runs
			SET durability_stage = ?, durability = ?,
			    verified_snapshot_id = ?,
			    synced_at = COALESCE(synced_at, ?),
			    unmount_verified_at = COALESCE(unmount_verified_at, ?),
			    durable_success_at = COALESCE(durable_success_at, ?)
			WHERE run_id = ? AND repository_id = ? AND disk_id = ?`,
			string(domain.StageDurableSuccess),
			string(backup.Durability),
			backup.VerifiedSnapshotID,
			nullableTime(backup.SyncedAt),
			nullableTime(backup.UnmountVerifiedAt),
			formatTime(backup.DurableSuccessAt),
			backup.RunID,
			backup.RepositoryID, backup.DiskID,
		)
		if err != nil {
			return fmt.Errorf("complete backup run: %w", err)
		}
		if err := requireBackupRunRow(result); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET status = ?, summary = ?, summary_json = ?,
			    last_error_class = NULL, last_error_code = NULL, last_safe_error = NULL,
			    started_at = ?, finished_at = ?, updated_at = ?
			WHERE id = ?`,
			string(run.Status),
			run.Summary,
			run.SummaryJSON,
			nullableTime(run.StartedAt),
			nullableTime(run.FinishedAt),
			formatTime(run.UpdatedAt),
			run.ID,
		); err != nil {
			return fmt.Errorf("complete backup run status: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE repositories
			SET last_snapshot_id = ?, last_successful_backup_at = ?,
			    health = CASE WHEN ? = 'NORMAL' THEN 'HEALTHY' ELSE health END,
			    suspect_reason = CASE WHEN ? = 'NORMAL' THEN NULL ELSE suspect_reason END,
			    last_opened_at = ?, updated_at = ?
			WHERE id = ?`,
			backup.VerifiedSnapshotID,
			formatTime(backup.DurableSuccessAt),
			string(backup.Durability), string(backup.Durability),
			formatTime(backup.DurableSuccessAt),
			formatTime(run.UpdatedAt),
			backup.RepositoryID,
		); err != nil {
			return fmt.Errorf("record repository backup: %w", err)
		}

		for _, item := range coverage {
			if item.BackupRunID != backup.RunID || item.RepositoryID != backup.RepositoryID ||
				item.DiskID != backup.DiskID || item.SnapshotID != backup.VerifiedSnapshotID {
				return domain.NewError(domain.ErrClassInvariant, "backup_coverage_mismatch", "coverage does not match the proven backup")
			}
			var included int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_revision_batches
				WHERE backup_run_id = ? AND revision_batch_id = ?`, backup.RunID, item.RevisionBatchID).Scan(&included); err != nil {
				return err
			}
			if included != 1 {
				return domain.NewError(domain.ErrClassInvariant, "backup_coverage_unread", "coverage requires a recorded revision read-set")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO revision_coverage (revision_batch_id, backup_run_id,
					repository_id, disk_id, snapshot_id, covered_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (revision_batch_id, backup_run_id) DO NOTHING`,
				item.RevisionBatchID,
				item.BackupRunID,
				item.RepositoryID,
				item.DiskID,
				item.SnapshotID,
				formatTime(item.CoveredAt),
			); err != nil {
				return fmt.Errorf("record revision coverage: %w", err)
			}
		}
		return nil
	})
}

// FailBackupRun records a terminal stage on a backup run that will not become a
// success. It never touches coverage: a run that did not prove its durability
// covers nothing.
func (s *Store) FailBackupRun(ctx context.Context, runID string, stage domain.DurabilityStage) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE backup_runs SET durability_stage = ? WHERE run_id = ?`,
			string(stage), runID)
		if err != nil {
			return fmt.Errorf("record backup failure: %w", err)
		}
		return requireBackupRunRow(result)
	})
}

// SaveReadSet writes the exact set of staging states and revision batches a
// backup run will read, in one transaction, before restic starts.
func (s *Store) SaveReadSet(ctx context.Context, runID string, states []domain.BackupSourceState,
	batches []domain.BackupRevisionBatch) error {

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, state := range states {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO backup_source_states (backup_run_id, source_id,
					current_collection_run_id, staging_state_version, published_at,
					current_relative_path, captured_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (backup_run_id, source_id) DO UPDATE SET
					current_collection_run_id = excluded.current_collection_run_id,
					staging_state_version = excluded.staging_state_version,
					published_at = excluded.published_at,
					current_relative_path = excluded.current_relative_path,
					captured_at = excluded.captured_at`,
				runID,
				state.SourceID,
				nullableText(state.CurrentCollectionRunID),
				state.StagingStateVersion,
				nullableTime(state.PublishedAt),
				state.CurrentRelativePath,
				formatTime(state.CapturedAt),
			); err != nil {
				return fmt.Errorf("record backup source state: %w", err)
			}
		}
		for _, batch := range batches {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO backup_revision_batches (backup_run_id, revision_batch_id,
					source_id, relative_path, batch_created_at, captured_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (backup_run_id, revision_batch_id) DO NOTHING`,
				runID,
				batch.RevisionBatchID,
				batch.SourceID,
				batch.RelativePath,
				formatTime(batch.BatchCreatedAt),
				formatTime(batch.CapturedAt),
			); err != nil {
				return fmt.Errorf("record backup revision batch: %w", err)
			}
		}
		return nil
	})
}

// BackupRun loads the backup detail of a run, or nil when the run is not a
// backup.
func (s *Store) BackupRun(ctx context.Context, runID string) (*domain.BackupRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+backupRunColumns+` FROM backup_runs WHERE run_id = ?`, runID)
	backup, err := scanBackupRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return backup, nil
}

// ListBackupRunsForRepository returns a repository's backup runs, newest first.
func (s *Store) ListBackupRunsForRepository(ctx context.Context, repositoryID string, limit int) ([]domain.BackupRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+backupRunColumns+`
		FROM backup_runs
		JOIN runs ON runs.id = backup_runs.run_id
		WHERE backup_runs.repository_id = ?
		ORDER BY runs.requested_at DESC, backup_runs.run_id DESC
		LIMIT ?`, repositoryID, limit)
	if err != nil {
		return nil, fmt.Errorf("list backup runs: %w", err)
	}
	defer rows.Close()
	return scanBackupRuns(rows)
}

// BackupRunsAwaitingRecovery returns every backup run of a repository whose
// restic phase started but which never reached a proven durable end.
//
// These are exactly the runs an explicit recovery has to resolve: each of them
// may have left a real snapshot behind that nobody has confirmed.
func (s *Store) BackupRunsAwaitingRecovery(ctx context.Context, repositoryID string) ([]domain.BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+backupRunColumns+`
		FROM backup_runs
		WHERE repository_id = ?
		  AND durability_stage IN (?, ?, ?, ?, ?)
		ORDER BY run_id`,
		repositoryID,
		string(domain.StageResticRunning),
		string(domain.StageSnapshotCreated),
		string(domain.StageSnapshotVerified),
		string(domain.StageSynced),
		string(domain.StageUnmountVerified),
	)
	if err != nil {
		return nil, fmt.Errorf("list backup runs awaiting recovery: %w", err)
	}
	defer rows.Close()
	return scanBackupRuns(rows)
}

// BackupSourceStates returns the exact staging states one backup run read.
func (s *Store) BackupSourceStates(ctx context.Context, runID string) ([]domain.BackupSourceState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backup_run_id, source_id, current_collection_run_id,
		       staging_state_version, published_at, current_relative_path, captured_at
		FROM backup_source_states
		WHERE backup_run_id = ?
		ORDER BY source_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list backup source states: %w", err)
	}
	defer rows.Close()

	var result []domain.BackupSourceState
	for rows.Next() {
		var state domain.BackupSourceState
		var collectionRunID sql.NullString
		var publishedAt, capturedAt sql.NullString
		if err := rows.Scan(&state.BackupRunID, &state.SourceID, &collectionRunID,
			&state.StagingStateVersion, &publishedAt, &state.CurrentRelativePath,
			&capturedAt); err != nil {
			return nil, fmt.Errorf("scan backup source state: %w", err)
		}
		state.CurrentCollectionRunID = collectionRunID.String
		var err error
		if state.PublishedAt, err = parseTime(publishedAt); err != nil {
			return nil, err
		}
		if state.CapturedAt, err = parseTime(capturedAt); err != nil {
			return nil, err
		}
		result = append(result, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup source states: %w", err)
	}
	return result, nil
}

// BackupRevisionBatches returns the exact revision batches one backup run read.
func (s *Store) BackupRevisionBatches(ctx context.Context, runID string) ([]domain.BackupRevisionBatch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backup_run_id, revision_batch_id, source_id, relative_path,
		       batch_created_at, captured_at
		FROM backup_revision_batches
		WHERE backup_run_id = ?
		ORDER BY source_id, batch_created_at`, runID)
	if err != nil {
		return nil, fmt.Errorf("list backup revision batches: %w", err)
	}
	defer rows.Close()

	var result []domain.BackupRevisionBatch
	for rows.Next() {
		var batch domain.BackupRevisionBatch
		var createdAt, capturedAt sql.NullString
		if err := rows.Scan(&batch.BackupRunID, &batch.RevisionBatchID, &batch.SourceID,
			&batch.RelativePath, &createdAt, &capturedAt); err != nil {
			return nil, fmt.Errorf("scan backup revision batch: %w", err)
		}
		var err error
		if batch.BatchCreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if batch.CapturedAt, err = parseTime(capturedAt); err != nil {
			return nil, err
		}
		result = append(result, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup revision batches: %w", err)
	}
	return result, nil
}

// RevisionCoverageForBatch returns every proven copy of one revision batch.
func (s *Store) RevisionCoverageForBatch(ctx context.Context, batchID string) ([]domain.RevisionCoverage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT revision_batch_id, backup_run_id, repository_id, disk_id,
		       snapshot_id, covered_at
		FROM revision_coverage
		WHERE revision_batch_id = ?
		ORDER BY covered_at DESC`, batchID)
	if err != nil {
		return nil, fmt.Errorf("list revision coverage: %w", err)
	}
	defer rows.Close()

	var result []domain.RevisionCoverage
	for rows.Next() {
		var item domain.RevisionCoverage
		var coveredAt sql.NullString
		if err := rows.Scan(&item.RevisionBatchID, &item.BackupRunID, &item.RepositoryID,
			&item.DiskID, &item.SnapshotID, &coveredAt); err != nil {
			return nil, fmt.Errorf("scan revision coverage: %w", err)
		}
		var err error
		if item.CoveredAt, err = parseTime(coveredAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate revision coverage: %w", err)
	}
	return result, nil
}

// ActiveRepositoryRuns returns every repository-touching run a previous process
// left non-terminal.
func (s *Store) ActiveRepositoryRuns(ctx context.Context) ([]domain.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE status IN (?, ?) AND kind IN (?, ?, ?)
		ORDER BY requested_at`,
		string(domain.RunStatusPending), string(domain.RunStatusRunning),
		string(domain.RunKindBackupLatest), string(domain.RunKindDiskSetup),
		string(domain.RunKindVerify))
	if err != nil {
		return nil, fmt.Errorf("list active repository runs: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

func scanBackupRuns(rows *sql.Rows) ([]domain.BackupRun, error) {
	var result []domain.BackupRun
	for rows.Next() {
		backup, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *backup)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup runs: %w", err)
	}
	return result, nil
}

func scanBackupRun(row interface{ Scan(...any) error }) (*domain.BackupRun, error) {
	var backup domain.BackupRun
	var stage, durability, selected string
	var candidate, verified sql.NullString
	var readSetAt, barrierAt, resticAt, verifiedAt, syncedAt, unmountAt, successAt sql.NullString

	if err := row.Scan(&backup.RunID, &backup.RepositoryID, &backup.DiskID, &backup.Semantics,
		&stage, &durability, &selected, &readSetAt, &barrierAt, &resticAt,
		&candidate, &verified, &verifiedAt, &syncedAt, &unmountAt, &successAt,
		&backup.FilesProcessed, &backup.FilesNew, &backup.FilesChanged,
		&backup.FilesUnmodified, &backup.BytesProcessed, &backup.BytesAdded,
		&backup.EngineErrors); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan backup run: %w", err)
	}

	backup.Stage = domain.DurabilityStage(stage)
	backup.Durability = domain.BackupDurability(durability)
	backup.CandidateSnapshotID = candidate.String
	backup.VerifiedSnapshotID = verified.String
	if err := json.Unmarshal([]byte(selected), &backup.SelectedSourceIDs); err != nil {
		return nil, fmt.Errorf("decode selected sources: %w", err)
	}

	for _, field := range []struct {
		raw    sql.NullString
		target *time.Time
	}{
		{readSetAt, &backup.ReadSetCapturedAt},
		{barrierAt, &backup.BarrierAcquiredAt},
		{resticAt, &backup.ResticStartedAt},
		{verifiedAt, &backup.SnapshotVerifiedAt},
		{syncedAt, &backup.SyncedAt},
		{unmountAt, &backup.UnmountVerifiedAt},
		{successAt, &backup.DurableSuccessAt},
	} {
		parsed, err := parseTime(field.raw)
		if err != nil {
			return nil, err
		}
		*field.target = parsed
	}
	return &backup, nil
}

func requireBackupRunRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update backup run: %w", err)
	}
	if affected == 0 {
		return domain.NewError(domain.ErrClassInvariant, "backup_run_missing",
			"the backup run being updated no longer exists")
	}
	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
