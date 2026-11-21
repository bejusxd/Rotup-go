package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// DispatchIntent is orchestration metadata read alongside the canonical run.
// Manual service updates intentionally do not rewrite these fields.
type DispatchIntent struct {
	Run             domain.Run
	Job             domain.Job
	NominalDueAt    string
	ParentRunID     string
	AttemptCount    int
	RetryNotBefore  time.Time
	RetryDeadlineAt time.Time
	WaitingReason   string
}

func (s *Store) JobSourceIDs(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id FROM job_sources WHERE job_id=? ORDER BY position,source_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) SetJobSources(ctx context.Context, jobID string, ids []string) error {
	if len(ids) == 0 {
		return domain.NewError(domain.ErrClassConfiguration, "job_sources_empty", "select at least one source")
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_sources WHERE job_id=?`, jobID); err != nil {
			return err
		}
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_sources(job_id,source_id,position) VALUES(?,?,?)`, jobID, id, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DispatchIntent(ctx context.Context, id string) (*DispatchIntent, error) {
	run, err := s.Run(ctx, id)
	if err != nil || run == nil {
		return nil, err
	}
	var jobID, nominal, parent, reason, retry, deadline sql.NullString
	item := &DispatchIntent{Run: *run}
	err = s.db.QueryRowContext(ctx, `SELECT job_id,nominal_due_at,parent_run_id,attempt_count,retry_not_before,retry_deadline_at,waiting_reason FROM runs WHERE id=?`, id).Scan(&jobID, &nominal, &parent, &item.AttemptCount, &retry, &deadline, &reason)
	if err != nil {
		return nil, err
	}
	if !jobID.Valid {
		return nil, nil
	}
	job, err := s.Job(ctx, jobID.String)
	if err != nil || job == nil {
		return nil, err
	}
	item.Job = *job
	item.NominalDueAt = nominal.String
	item.ParentRunID = parent.String
	item.WaitingReason = reason.String
	if item.RetryNotBefore, err = parseTime(retry); err != nil {
		return nil, err
	}
	if item.RetryDeadlineAt, err = parseTime(deadline); err != nil {
		return nil, err
	}
	return item, nil
}

// DispatchQueue returns the oldest unfinished occurrence per enabled job.
// SQLite rowid is insertion order: timestamps with fractional seconds must not
// be lexically ordered, and equal requested times must not reorder occurrences.
func (s *Store) DispatchQueue(ctx context.Context) ([]DispatchIntent, error) {
	active, args := activeStatusFilter()
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM runs r JOIN jobs j ON j.id=r.job_id
	 WHERE j.enabled=1 AND r.status IN ('PENDING','WAITING_FOR_RESOURCE','RETRY_DELAY','QUEUED')
	 AND NOT EXISTS(SELECT 1 FROM runs old WHERE old.job_id=r.job_id AND old.rowid<r.rowid AND old.status IN (`+active+`))
	 ORDER BY r.priority DESC,r.rowid`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []DispatchIntent
	for _, id := range ids {
		item, err := s.DispatchIntent(ctx, id)
		if err != nil {
			return nil, err
		}
		if item != nil {
			result = append(result, *item)
		}
	}
	return result, nil
}

// ClaimJobAttempt is a short compare-and-swap, never a lock held across work.
func (s *Store) ClaimJobAttempt(ctx context.Context, item DispatchIntent, now time.Time) (bool, error) {
	claimed := false
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var notBefore, limit sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT retry_not_before,retry_deadline_at FROM runs WHERE id=?`, item.Run.ID).Scan(&notBefore, &limit); err != nil {
			return err
		}
		retryAt, err := parseTime(notBefore)
		if err != nil {
			return err
		}
		limitAt, err := parseTime(limit)
		if err != nil {
			return err
		}
		if now.Before(retryAt) || (!limitAt.IsZero() && !now.Before(limitAt)) {
			return nil
		}
		deadline := item.RetryDeadlineAt
		if deadline.IsZero() && item.Job.RetriesAutomatically() {
			deadline = now.Add(time.Duration(*item.Job.RetryDeadlineSeconds) * time.Second)
		}
		active, args := activeStatusFilter()
		params := []any{formatTime(now), nullableTime(deadline), formatTime(now), item.Run.ID, string(item.Run.Status), item.AttemptCount}
		params = append(params, args...)
		result, err := tx.ExecContext(ctx, `UPDATE runs SET status='RUNNING',attempt_count=attempt_count+1,
		 started_at=COALESCE(started_at,?),retry_deadline_at=?,retry_not_before=NULL,waiting_reason=NULL,finished_at=NULL,updated_at=?
		 WHERE id=? AND status=? AND attempt_count=? AND status IN ('PENDING','WAITING_FOR_RESOURCE','QUEUED','RETRY_DELAY')
		 AND EXISTS(SELECT 1 FROM jobs WHERE jobs.id=runs.job_id AND enabled=1)
		 AND NOT EXISTS(SELECT 1 FROM runs old WHERE old.job_id=runs.job_id AND old.rowid<runs.rowid AND old.status IN (`+active+`))`, params...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		claimed = n == 1
		return err
	})
	return claimed, err
}

// SetDispatchState changes only an observed generation. It cannot overwrite a
// worker that has claimed a newer attempt or a concurrent cancellation.
func (s *Store) SetDispatchState(ctx context.Context, item DispatchIntent, run domain.Run, reason string, retry time.Time) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=?,last_error_class=?,last_error_code=?,last_safe_error=?,
		 waiting_reason=?,retry_not_before=?,finished_at=?,updated_at=? WHERE id=? AND status=? AND attempt_count=?`,
			string(run.Status), run.Summary, nullableText(string(run.ErrorClass)), nullableText(run.ErrorCode), nullableText(run.SafeError),
			nullableText(reason), nullableTime(retry), nullableTime(run.FinishedAt), formatTime(time.Now().UTC()), run.ID, string(item.Run.Status), item.AttemptCount)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("dispatch state changed concurrently for %s", run.ID)
		}
		return nil
	})
}

// ResumeUntouchedIntents runs only after the ordinary startup sweep. An intent
// with no attempt, no start and no source/backup result has performed no work.
// Preserve its original ID and nominal occurrence; never resurrect restic work.
func (s *Store) ResumeUntouchedIntents(ctx context.Context) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET status='PENDING',finished_at=NULL,summary='queued after restart',
		 last_error_class=NULL,last_error_code=NULL,last_safe_error=NULL,waiting_reason=NULL
		 WHERE job_id IS NOT NULL AND status='INTERRUPTED' AND last_error_code IN ('daemon_restarted','repository_interrupted')
		 AND attempt_count=0 AND started_at IS NULL
		 AND NOT EXISTS(SELECT 1 FROM backup_runs b WHERE b.run_id=runs.id)
		 AND NOT EXISTS(SELECT 1 FROM run_source_results s WHERE s.collection_run_id=runs.id)`)
		return err
	})
}

func (s *Store) DispatchSourceIDs(ctx context.Context, item DispatchIntent) ([]string, error) {
	if item.ParentRunID == "" {
		return s.JobSourceIDs(ctx, item.Job.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_id FROM run_source_results WHERE collection_run_id=?
	 AND status IN ('SUCCEEDED','SUCCEEDED_WITH_WARNING') AND staging_state_version>0 AND published_at IS NOT NULL ORDER BY source_id`, item.ParentRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Never repeat an ambiguous repository write or a collection that published or
// mutated staging. Preflight-only transient failures may reuse the same intent.
func (s *Store) JobRetrySafe(ctx context.Context, runID string) (bool, error) {
	var unsafe bool
	err := s.db.QueryRowContext(ctx, `SELECT
	 EXISTS(SELECT 1 FROM backup_runs WHERE run_id=?) OR
	 EXISTS(SELECT 1 FROM revision_batches WHERE collection_run_id=?) OR
	 EXISTS(SELECT 1 FROM run_source_results r JOIN sources s ON s.id=r.source_id
	 WHERE r.collection_run_id=? AND (r.published_at IS NOT NULL OR s.staging_status IN ('UPDATING','RECOVERY_REQUIRED')))`, runID, runID, runID).Scan(&unsafe)
	return !unsafe, err
}

func (s *Store) EnqueueChainedBackup(ctx context.Context, parentID string, now time.Time) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,parent_run_id,kind,trigger_kind,status,summary,requested_at,updated_at,idempotency_key)
		 SELECT ?,r.job_id,r.id,'BACKUP_LATEST','COLLECTION_SUCCESS','PENDING','backup after collection',?,?,?
		 FROM runs r JOIN jobs j ON j.id=r.job_id
		 WHERE r.id=? AND r.kind='COLLECTION' AND j.backup_after_collection=1
		 AND (r.status IN ('SUCCEEDED','SUCCEEDED_WITH_WARNING') OR (r.status='PARTIAL' AND j.partial_triggers_backup=1))
		 AND EXISTS(SELECT 1 FROM run_source_results x WHERE x.collection_run_id=r.id AND x.status IN ('SUCCEEDED','SUCCEEDED_WITH_WARNING') AND x.published_at IS NOT NULL)
		 ON CONFLICT(idempotency_key) DO NOTHING`, id, formatTime(now), formatTime(now), "collection-success:"+parentID, parentID)
		return err
	})
}

// Repair the crash gap after a committed collection outcome, without relying
// on a callback having run. The unique key makes this idempotent.
func (s *Store) ReconcileJobChains(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM runs r JOIN jobs j ON j.id=r.job_id
	 WHERE r.kind='COLLECTION' AND j.backup_after_collection=1
	 AND (r.status IN ('SUCCEEDED','SUCCEEDED_WITH_WARNING') OR (r.status='PARTIAL' AND j.partial_triggers_backup=1))
	 AND NOT EXISTS(SELECT 1 FROM runs child WHERE child.idempotency_key='collection-success:'||r.id)`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.EnqueueChainedBackup(ctx, id, now); err != nil {
			return err
		}
	}
	return nil
}
