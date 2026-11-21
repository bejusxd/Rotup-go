package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

type JobDefinition struct {
	Job       domain.Job
	Schedules []domain.Schedule
	SourceIDs []string
}

// CreateJobDefinition commits configuration and source membership together;
// incomplete jobs never become visible to the planner.
func (s *Store) CreateJobDefinition(ctx context.Context, j domain.Job, sch domain.Schedule, ids []string) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Name, string(j.Kind), boolToInt(j.Enabled), boolToInt(j.BackupAfterCollection), boolToInt(j.PartialTriggersBackup), j.TimeoutSeconds, j.RetryIntervalSeconds, nullableInt64(j.RetryDeadlineSeconds), "{}", formatTime(j.CreatedAt), formatTime(j.UpdatedAt)); err != nil {
			return err
		}
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO job_sources(job_id,source_id,position)VALUES(?,?,?)`, j.ID, id, i); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO schedules (`+scheduleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?)`, sch.ID, j.ID, boolToInt(sch.Enabled), int64(sch.DaysOfWeekMask), sch.LocalTime, sch.Timezone, nil, nullableTime(instantOf(sch.NextRunAt)), formatTime(sch.CreatedAt), formatTime(sch.UpdatedAt))
		return err
	})
}

func (s *Store) ListJobDefinitions(ctx context.Context) ([]JobDefinition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs ORDER BY name,id`)
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
	result := make([]JobDefinition, 0, len(ids))
	for _, id := range ids {
		job, err := s.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		if job == nil {
			continue
		}
		sources, err := s.JobSourceIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		definition := JobDefinition{Job: *job, SourceIDs: sources, Schedules: []domain.Schedule{}}
		rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE job_id=? ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			sch, err := scanSchedule(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			definition.Schedules = append(definition.Schedules, *sch)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		result = append(result, definition)
	}
	return result, nil
}

func (s *Store) SetJobEnabled(ctx context.Context, id string, enabled bool) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE jobs SET enabled=?,updated_at=? WHERE id=?`, boolToInt(enabled), formatTime(time.Now().UTC()), id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return domain.NewError(domain.ErrClassConfiguration, "job_missing", "the job does not exist")
		}
		return nil
	})
}

func (s *Store) EnqueueJobRun(ctx context.Context, jobID string, now time.Time) (string, error) {
	id, err := domain.NewID()
	if err != nil {
		return "", err
	}
	err = s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,kind,trigger_kind,status,summary,requested_at,updated_at)
		 SELECT ?,id,kind,'MANUAL','PENDING','manual job request',?,? FROM jobs WHERE id=? AND enabled=1 AND kind IN ('COLLECTION','BACKUP_LATEST')`, id, formatTime(now), formatTime(now), jobID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return domain.NewError(domain.ErrClassConfiguration, "job_unavailable", "enable an existing supported job before requesting a run")
		}
		return nil
	})
	return id, err
}
