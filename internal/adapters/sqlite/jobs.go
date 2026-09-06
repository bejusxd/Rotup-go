package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const jobColumns = `id, name, kind, enabled, backup_after_collection,
	partial_triggers_backup, timeout_seconds, retry_interval_seconds,
	retry_deadline_seconds, options_json, created_at, updated_at`

const scheduleColumns = `id, job_id, enabled, days_of_week_mask, local_time, timezone,
	last_nominal_run_at, next_run_at, created_at, updated_at`

// CreateJob stores one job.
func (s *Store) CreateJob(ctx context.Context, job domain.Job) error {
	if job.OptionsJSON == "" {
		job.OptionsJSON = "{}"
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (`+jobColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			job.ID, job.Name, string(job.Kind), boolToInt(job.Enabled),
			boolToInt(job.BackupAfterCollection), boolToInt(job.PartialTriggersBackup),
			job.TimeoutSeconds, job.RetryIntervalSeconds,
			nullableInt64(job.RetryDeadlineSeconds), job.OptionsJSON,
			formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
		if err != nil {
			return fmt.Errorf("create job %q: %w", job.ID, err)
		}
		return nil
	})
}

// CreateSchedule stores one schedule.
func (s *Store) CreateSchedule(ctx context.Context, schedule domain.Schedule) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO schedules (`+scheduleColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			schedule.ID, schedule.JobID, boolToInt(schedule.Enabled),
			int64(schedule.DaysOfWeekMask), schedule.LocalTime, schedule.Timezone,
			nullableText(schedule.LastNominalRunAt), nullableTime(instantOf(schedule.NextRunAt)),
			formatTime(schedule.CreatedAt), formatTime(schedule.UpdatedAt))
		if err != nil {
			return fmt.Errorf("create schedule %q: %w", schedule.ID, err)
		}
		return nil
	})
}

// Job loads one job, or nil when it does not exist.
func (s *Store) Job(ctx context.Context, id string) (*domain.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

// Schedule loads one schedule, or nil when it does not exist.
func (s *Store) Schedule(ctx context.Context, id string) (*domain.Schedule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id)
	schedule, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return schedule, err
}

// ScheduledJob pairs a schedule with the job it belongs to.
type ScheduledJob struct {
	Schedule domain.Schedule
	Job      domain.Job
}

// ActiveSchedules lists every schedule that could fire: the schedule and its job
// must both be enabled.
//
// A daemon with no jobs configured gets an empty list and therefore does
// nothing, which is what makes automation opt-in rather than something an
// upgrade switches on.
func (s *Store) ActiveSchedules(ctx context.Context) ([]ScheduledJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed("s", scheduleColumns)+`, `+prefixed("j", jobColumns)+`
		FROM schedules s
		JOIN jobs j ON j.id = s.job_id
		WHERE s.enabled = 1 AND j.enabled = 1
		ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("list active schedules: %w", err)
	}
	defer rows.Close()

	var result []ScheduledJob
	for rows.Next() {
		var pair ScheduledJob
		if err := scanScheduleAndJob(rows, &pair); err != nil {
			return nil, err
		}
		result = append(result, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active schedules: %w", err)
	}
	return result, nil
}

// ScheduledIntent is one durable run to create for a due occurrence, together
// with the schedule bookkeeping that must move with it.
type ScheduledIntent struct {
	Run domain.Run
	// IdempotencyKey is what makes creating the same occurrence twice
	// impossible, whichever process or goroutine gets there first.
	IdempotencyKey string
	ScheduleID     string
	JobID          string
	// NominalDueAt is the local due identity, in canonical form.
	NominalDueAt string
	// NextRunAt is where the schedule's bookkeeping moves to.
	NextRunAt time.Time
}

// ClaimScheduledRun creates a scheduled run and advances its schedule in one
// transaction, and reports whether this caller was the one that created it.
//
// Both halves must move together. A run created without the bookkeeping
// advancing would be re-attempted on the next tick forever; bookkeeping advanced
// without the run is worse, because the due time is simply lost and no operator
// would see that anything failed to happen. The transaction is what rules both
// out.
//
// A duplicate key means someone else already claimed this occurrence. That is
// not an error: the bookkeeping still advances, because it advances to exactly
// the values the winning caller computed, and leaving it behind would make this
// schedule retry a due time that has already been claimed.
func (s *Store) ClaimScheduledRun(ctx context.Context, intent ScheduledIntent) (bool, error) {
	claimed := false
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		claimed = false
		run := intent.Run
		if run.SummaryJSON == "" {
			run.SummaryJSON = "{}"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO runs (id, job_id, schedule_id, kind, trigger_kind, source_id,
				nominal_due_at, idempotency_key, status, summary, summary_json,
				requested_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(idempotency_key) DO NOTHING`,
			run.ID, intent.JobID, intent.ScheduleID, string(run.Kind),
			string(run.TriggerKind), nullableText(run.SourceID),
			intent.NominalDueAt, intent.IdempotencyKey, string(run.Status),
			run.Summary, run.SummaryJSON,
			formatTime(run.RequestedAt), formatTime(run.UpdatedAt))
		if err != nil {
			return fmt.Errorf("claim scheduled run %q: %w", run.ID, err)
		}
		// Whether this caller won is decided by whether the row it tried to
		// insert is the row that is there, not by a rows-affected count that a
		// future ON CONFLICT clause could change the meaning of.
		var ownerID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM runs WHERE idempotency_key = ?`, intent.IdempotencyKey).
			Scan(&ownerID); err != nil {
			return fmt.Errorf("read the claimed run: %w", err)
		}
		claimed = ownerID == run.ID

		result, err := tx.ExecContext(ctx, `
			UPDATE schedules
			SET last_nominal_run_at = ?, next_run_at = ?, updated_at = ?
			WHERE id = ?`,
			intent.NominalDueAt, formatTime(intent.NextRunAt),
			formatTime(run.UpdatedAt), intent.ScheduleID)
		if err != nil {
			return fmt.Errorf("advance schedule %q: %w", intent.ScheduleID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("confirm schedule %q advanced: %w", intent.ScheduleID, err)
		}
		if affected != 1 {
			// The schedule vanished under us. Rolling back is the only safe
			// answer: committing the run alone would leave work nothing owns.
			return fmt.Errorf("schedule %q no longer exists", intent.ScheduleID)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// SetScheduleNextRun updates only the cached next occurrence. Reconciliation
// uses it to recompute due times without claiming anything.
func (s *Store) SetScheduleNextRun(ctx context.Context, scheduleID string, nextRunAt time.Time,
	updatedAt time.Time) error {

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE schedules SET next_run_at = ?, updated_at = ? WHERE id = ?`,
			formatTime(nextRunAt), formatTime(updatedAt), scheduleID)
		if err != nil {
			return fmt.Errorf("set next run for schedule %q: %w", scheduleID, err)
		}
		return nil
	})
}

func scanJob(row interface{ Scan(...any) error }) (*domain.Job, error) {
	var job domain.Job
	var kind string
	var enabled, backupAfter, partialTriggers int
	var retryDeadline sql.NullInt64
	var createdAt, updatedAt sql.NullString
	if err := row.Scan(&job.ID, &job.Name, &kind, &enabled, &backupAfter,
		&partialTriggers, &job.TimeoutSeconds, &job.RetryIntervalSeconds,
		&retryDeadline, &job.OptionsJSON, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	job.Kind = domain.JobKind(kind)
	job.Enabled = enabled == 1
	job.BackupAfterCollection = backupAfter == 1
	job.PartialTriggersBackup = partialTriggers == 1
	if retryDeadline.Valid {
		value := retryDeadline.Int64
		job.RetryDeadlineSeconds = &value
	}
	var err error
	if job.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if job.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &job, nil
}

func scanSchedule(row interface{ Scan(...any) error }) (*domain.Schedule, error) {
	var schedule domain.Schedule
	var enabled int
	var mask int64
	var lastNominal, nextRunAt sql.NullString
	var createdAt, updatedAt sql.NullString
	if err := row.Scan(&schedule.ID, &schedule.JobID, &enabled, &mask,
		&schedule.LocalTime, &schedule.Timezone, &lastNominal, &nextRunAt,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	schedule.Enabled = enabled == 1
	schedule.DaysOfWeekMask = uint8(mask)
	schedule.LastNominalRunAt = lastNominal.String
	if nextRunAt.Valid {
		parsed, err := parseTime(nextRunAt)
		if err != nil {
			return nil, err
		}
		schedule.NextRunAt = &parsed
	}
	var err error
	if schedule.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if schedule.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &schedule, nil
}

func scanScheduleAndJob(rows *sql.Rows, pair *ScheduledJob) error {
	var enabled, jobEnabled, backupAfter, partialTriggers int
	var mask int64
	var lastNominal, nextRunAt sql.NullString
	var scheduleCreated, scheduleUpdated, jobCreated, jobUpdated sql.NullString
	var kind string
	var retryDeadline sql.NullInt64

	if err := rows.Scan(
		&pair.Schedule.ID, &pair.Schedule.JobID, &enabled, &mask,
		&pair.Schedule.LocalTime, &pair.Schedule.Timezone, &lastNominal, &nextRunAt,
		&scheduleCreated, &scheduleUpdated,
		&pair.Job.ID, &pair.Job.Name, &kind, &jobEnabled, &backupAfter,
		&partialTriggers, &pair.Job.TimeoutSeconds, &pair.Job.RetryIntervalSeconds,
		&retryDeadline, &pair.Job.OptionsJSON, &jobCreated, &jobUpdated); err != nil {
		return fmt.Errorf("scan scheduled job: %w", err)
	}

	pair.Schedule.Enabled = enabled == 1
	pair.Schedule.DaysOfWeekMask = uint8(mask)
	pair.Schedule.LastNominalRunAt = lastNominal.String
	if nextRunAt.Valid {
		parsed, err := parseTime(nextRunAt)
		if err != nil {
			return err
		}
		pair.Schedule.NextRunAt = &parsed
	}
	pair.Job.Kind = domain.JobKind(kind)
	pair.Job.Enabled = jobEnabled == 1
	pair.Job.BackupAfterCollection = backupAfter == 1
	pair.Job.PartialTriggersBackup = partialTriggers == 1
	if retryDeadline.Valid {
		value := retryDeadline.Int64
		pair.Job.RetryDeadlineSeconds = &value
	}
	var err error
	if pair.Schedule.CreatedAt, err = parseTime(scheduleCreated); err != nil {
		return err
	}
	if pair.Schedule.UpdatedAt, err = parseTime(scheduleUpdated); err != nil {
		return err
	}
	if pair.Job.CreatedAt, err = parseTime(jobCreated); err != nil {
		return err
	}
	if pair.Job.UpdatedAt, err = parseTime(jobUpdated); err != nil {
		return err
	}
	return nil
}

// prefixed qualifies a column list with a table alias, so two lists can be
// selected together without ambiguity.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func instantOf(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
