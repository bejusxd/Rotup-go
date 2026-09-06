package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const runColumns = `id, kind, trigger_kind, source_id, status, summary, summary_json,
	last_error_class, last_error_code, last_safe_error,
	requested_at, started_at, finished_at, updated_at`

// activeStatusFilter builds the IN clause for "this run has not finished" from
// the domain's own classification rather than from a list written out here.
//
// The literal version of this clause named PENDING and RUNNING, which was
// correct while those were the only active statuses. Widening the state machine
// without widening the clause would have been silent: a run left QUEUED or
// RETRY_DELAY by a crash would never be swept, and its repository would never be
// marked suspect. Deriving it means adding a status cannot reintroduce that.
func activeStatusFilter() (string, []any) {
	statuses := domain.ActiveRunStatuses()
	placeholders := make([]string, 0, len(statuses))
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		placeholders = append(placeholders, "?")
		args = append(args, string(status))
	}
	return strings.Join(placeholders, ", "), args
}

// RecordRun stores one complete run in a single statement. It is used for runs
// that are already terminal when they are written, such as the M1 demo run.
func (s *Store) RecordRun(ctx context.Context, run domain.Run) error {
	if run.TriggerKind == "" {
		run.TriggerKind = domain.RunTriggerManual
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.FinishedAt
	}
	if run.SummaryJSON == "" {
		run.SummaryJSON = "{}"
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO runs (`+runColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.ID,
			string(run.Kind),
			string(run.TriggerKind),
			nullableText(run.SourceID),
			string(run.Status),
			run.Summary,
			run.SummaryJSON,
			nullableText(string(run.ErrorClass)),
			nullableText(run.ErrorCode),
			nullableText(run.SafeError),
			formatTime(run.RequestedAt),
			nullableTime(run.StartedAt),
			nullableTime(run.FinishedAt),
			formatTime(run.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("record run %q: %w", run.ID, err)
		}
		return nil
	})
}

// UpdateRun rewrites the mutable fields of an existing run.
func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	run.UpdatedAt = time.Now().UTC()
	if run.SummaryJSON == "" {
		run.SummaryJSON = "{}"
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET status = ?, summary = ?, summary_json = ?,
			    last_error_class = ?, last_error_code = ?, last_safe_error = ?,
			    started_at = ?, finished_at = ?, updated_at = ?
			WHERE id = ?`,
			string(run.Status),
			run.Summary,
			run.SummaryJSON,
			nullableText(string(run.ErrorClass)),
			nullableText(run.ErrorCode),
			nullableText(run.SafeError),
			nullableTime(run.StartedAt),
			nullableTime(run.FinishedAt),
			formatTime(run.UpdatedAt),
			run.ID,
		)
		if err != nil {
			return fmt.Errorf("update run %q: %w", run.ID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("update run %q: %w", run.ID, err)
		}
		if affected == 0 {
			return domain.NewError(domain.ErrClassInvariant, "run_missing",
				"the run being updated no longer exists")
		}
		return nil
	})
}

// Run loads a single run by identifier.
func (s *Store) Run(ctx context.Context, id string) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// LatestRun returns the most recently requested run, or nil when history is
// empty. The M1 dashboard renders this.
func (s *Store) LatestRun(ctx context.Context) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		ORDER BY requested_at DESC, id DESC
		LIMIT 1`)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// ListRuns returns the most recent runs, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]domain.Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		ORDER BY requested_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

// ListRunsForSource returns the most recent runs of one source, newest first.
func (s *Store) ListRunsForSource(ctx context.Context, sourceID string, limit int) ([]domain.Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE source_id = ?
		ORDER BY requested_at DESC, id DESC
		LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs for source: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

// LatestCollectionForSource returns the newest collection run of a source.
func (s *Store) LatestCollectionForSource(ctx context.Context, sourceID string) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE source_id = ? AND kind = ?
		ORDER BY requested_at DESC, id DESC
		LIMIT 1`, sourceID, string(domain.RunKindCollection))
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return run, nil
}

// ActiveRuns returns every run that a previous process left non-terminal.
func (s *Store) ActiveRuns(ctx context.Context) ([]domain.Run, error) {
	placeholders, args := activeStatusFilter()
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE status IN (`+placeholders+`)
		ORDER BY requested_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

// SaveSourceResult writes the per-source outcome of a collection run and, in the
// same transaction, advances the source's durable staging state. Publication is
// therefore atomic from the database's point of view: a source can never be
// left claiming a state version that has no matching result row.
func (s *Store) SaveSourceResult(ctx context.Context, result domain.SourceResult, source *domain.Source) error {
	warnings, err := json.Marshal(boundedWarnings(result.Warnings))
	if err != nil {
		return fmt.Errorf("encode warnings: %w", err)
	}
	var stateVersion any
	if result.StagingStateVersion > 0 {
		stateVersion = result.StagingStateVersion
	}

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_source_results (
				collection_run_id, source_id, status, staging_state_version,
				published_at, started_at, finished_at,
				bytes_scanned, bytes_copied, files_scanned, files_copied,
				files_deleted_or_archived, engine_checks, engine_errors, warnings_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (collection_run_id, source_id) DO UPDATE SET
				status = excluded.status,
				staging_state_version = excluded.staging_state_version,
				published_at = excluded.published_at,
				started_at = excluded.started_at,
				finished_at = excluded.finished_at,
				bytes_scanned = excluded.bytes_scanned,
				bytes_copied = excluded.bytes_copied,
				files_scanned = excluded.files_scanned,
				files_copied = excluded.files_copied,
				files_deleted_or_archived = excluded.files_deleted_or_archived,
				engine_checks = excluded.engine_checks,
				engine_errors = excluded.engine_errors,
				warnings_json = excluded.warnings_json`,
			result.RunID,
			result.SourceID,
			string(result.Status),
			stateVersion,
			nullableTime(result.PublishedAt),
			nullableTime(result.StartedAt),
			nullableTime(result.FinishedAt),
			result.BytesScanned,
			result.BytesCopied,
			result.FilesScanned,
			result.FilesCopied,
			result.FilesDeletedOrArchived,
			result.EngineChecks,
			result.EngineErrors,
			string(warnings),
		); err != nil {
			return fmt.Errorf("save source result: %w", err)
		}

		if source == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sources
			SET staging_status = ?, staging_state_version = ?,
			    last_published_at = ?, last_collection_run_id = ?, updated_at = ?
			WHERE id = ?`,
			string(source.StagingStatus),
			source.StagingStateVersion,
			nullableTime(timeOrZero(source.LastPublishedAt)),
			nullableText(source.LastCollectionRunID),
			formatTime(time.Now().UTC()),
			source.ID,
		); err != nil {
			return fmt.Errorf("advance source staging state: %w", err)
		}
		return nil
	})
}

// SourceResult loads the stored per-source result of a collection run.
func (s *Store) SourceResult(ctx context.Context, runID, sourceID string) (*domain.SourceResult, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT collection_run_id, source_id, status, staging_state_version,
		       published_at, started_at, finished_at,
		       bytes_scanned, bytes_copied, files_scanned, files_copied,
		       files_deleted_or_archived, engine_checks, engine_errors, warnings_json
		FROM run_source_results
		WHERE collection_run_id = ? AND source_id = ?`, runID, sourceID)

	var result domain.SourceResult
	var status string
	var stateVersion sql.NullInt64
	var publishedAt, startedAt, finishedAt sql.NullString
	var warnings string
	err := row.Scan(&result.RunID, &result.SourceID, &status, &stateVersion,
		&publishedAt, &startedAt, &finishedAt,
		&result.BytesScanned, &result.BytesCopied, &result.FilesScanned, &result.FilesCopied,
		&result.FilesDeletedOrArchived, &result.EngineChecks, &result.EngineErrors, &warnings)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read source result: %w", err)
	}

	result.Status = domain.RunStatus(status)
	result.StagingStateVersion = stateVersion.Int64
	if result.PublishedAt, err = parseTime(publishedAt); err != nil {
		return nil, err
	}
	if result.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, err
	}
	if result.FinishedAt, err = parseTime(finishedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(warnings), &result.Warnings); err != nil {
		return nil, fmt.Errorf("decode warnings: %w", err)
	}
	return &result, nil
}

func scanRun(row interface{ Scan(...any) error }) (*domain.Run, error) {
	var run domain.Run
	var kind, triggerKind, status string
	var sourceID, errorClass, errorCode, safeError sql.NullString
	var requestedAt, startedAt, finishedAt, updatedAt sql.NullString

	if err := row.Scan(&run.ID, &kind, &triggerKind, &sourceID, &status,
		&run.Summary, &run.SummaryJSON, &errorClass, &errorCode, &safeError,
		&requestedAt, &startedAt, &finishedAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan run: %w", err)
	}

	run.Kind = domain.RunKind(kind)
	run.TriggerKind = domain.RunTrigger(triggerKind)
	run.SourceID = sourceID.String
	run.Status = domain.RunStatus(status)
	run.ErrorClass = domain.ErrorClass(errorClass.String)
	run.ErrorCode = errorCode.String
	run.SafeError = safeError.String

	var err error
	if run.RequestedAt, err = parseTime(requestedAt); err != nil {
		return nil, err
	}
	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return nil, err
	}
	if run.FinishedAt, err = parseTime(finishedAt); err != nil {
		return nil, err
	}
	if run.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &run, nil
}

func scanRuns(rows *sql.Rows) ([]domain.Run, error) {
	var result []domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return result, nil
}

// maxStoredWarnings bounds how much advisory text one result may keep.
const maxStoredWarnings = 20

func boundedWarnings(warnings []string) []string {
	if warnings == nil {
		return []string{}
	}
	if len(warnings) > maxStoredWarnings {
		return warnings[:maxStoredWarnings]
	}
	return warnings
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
