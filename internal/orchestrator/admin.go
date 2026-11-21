package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/schedule"
)

// JobInput is the operator's job definition. Zero timeout and retry interval
// mean "use the daemon defaults"; a nil retry deadline means one attempt.
type JobInput struct {
	Name                  string         `json:"name"`
	Kind                  domain.JobKind `json:"kind"`
	Enabled               bool           `json:"enabled"`
	SourceIDs             []string       `json:"sourceIds"`
	BackupAfterCollection bool           `json:"backupAfterCollection"`
	PartialTriggersBackup bool           `json:"partialTriggersBackup"`
	TimeoutSeconds        int64          `json:"timeoutSeconds"`
	RetryIntervalSeconds  int64          `json:"retryIntervalSeconds"`
	RetryDeadlineSeconds  *int64         `json:"retryDeadlineSeconds"`
	ScheduleEnabled       bool           `json:"scheduleEnabled"`
	DaysOfWeekMask        uint8          `json:"daysOfWeekMask"`
	LocalTime             string         `json:"localTime"`
	Timezone              string         `json:"timezone"`
}

// JobDefinition is the read model handed to presentation code, so the web
// layer never depends on the SQLite adapter's types.
type JobDefinition struct {
	Job       domain.Job
	Schedules []domain.Schedule
	SourceIDs []string
}

// DefaultRetryIntervalSeconds is the §18 schema default.
const DefaultRetryIntervalSeconds = 300

// Admin is the job configuration use case.
type Admin struct {
	Store *sqlite.Store
	// CollectionTimeout and BackupTimeout seed a job's timeout when the operator
	// leaves it unset, so an automated run is bounded exactly like the manual
	// run of the same kind.
	CollectionTimeout time.Duration
	BackupTimeout     time.Duration
	Now               func() time.Time
}

func (a *Admin) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func invalidJob(message string) error {
	return domain.NewError(domain.ErrClassConfiguration, "job_configuration_invalid", message)
}

// Create validates and commits a job, its source membership and its schedule
// in one transaction. A schedule is always stored, disabled when not wanted,
// so a job can be run on demand without ever firing automatically.
func (a *Admin) Create(ctx context.Context, in JobInput) (string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		return "", invalidJob("job name must contain between 1 and 120 bytes")
	}
	if in.Kind != domain.JobKindCollection && in.Kind != domain.JobKindBackupLatest {
		return "", invalidJob("only collection and backup jobs are supported")
	}
	if len(in.SourceIDs) == 0 || len(in.SourceIDs) > 100 {
		return "", invalidJob("select between 1 and 100 sources")
	}
	seen := map[string]bool{}
	for _, id := range in.SourceIDs {
		if seen[id] {
			return "", invalidJob("duplicate source selection")
		}
		seen[id] = true
		source, err := a.Store.Source(ctx, id)
		if err != nil {
			return "", err
		}
		if source == nil || !source.Enabled {
			return "", invalidJob("all selected sources must exist and be enabled")
		}
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = int64(a.defaultTimeout(in.Kind) / time.Second)
	}
	if in.RetryIntervalSeconds == 0 {
		in.RetryIntervalSeconds = DefaultRetryIntervalSeconds
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 7*86400 {
		return "", invalidJob("timeout must be between one second and seven days")
	}
	if in.RetryIntervalSeconds < 1 || in.RetryIntervalSeconds > 7*86400 {
		return "", invalidJob("retry interval must be between one second and seven days")
	}
	if in.RetryDeadlineSeconds != nil && (*in.RetryDeadlineSeconds < 1 || *in.RetryDeadlineSeconds > 30*86400) {
		return "", invalidJob("retry deadline must be between one second and 30 days, or absent for one attempt")
	}
	if in.Kind != domain.JobKindCollection && (in.BackupAfterCollection || in.PartialTriggersBackup) {
		return "", invalidJob("only collection jobs may chain a backup")
	}
	spec := schedule.Spec{Days: schedule.Weekdays(in.DaysOfWeekMask), LocalTime: in.LocalTime,
		Timezone: in.Timezone, Enabled: in.ScheduleEnabled}
	if err := spec.Validate(); err != nil {
		return "", err
	}

	now := a.now()
	id, err := domain.NewID()
	if err != nil {
		return "", err
	}
	scheduleID, err := domain.NewID()
	if err != nil {
		return "", err
	}
	job := domain.Job{ID: id, Name: in.Name, Kind: in.Kind, Enabled: in.Enabled,
		BackupAfterCollection: in.BackupAfterCollection, PartialTriggersBackup: in.PartialTriggersBackup,
		TimeoutSeconds: in.TimeoutSeconds, RetryIntervalSeconds: in.RetryIntervalSeconds,
		RetryDeadlineSeconds: in.RetryDeadlineSeconds, CreatedAt: now, UpdatedAt: now}
	sch := domain.Schedule{ID: scheduleID, JobID: id, Enabled: in.ScheduleEnabled,
		DaysOfWeekMask: in.DaysOfWeekMask, LocalTime: in.LocalTime, Timezone: in.Timezone,
		CreatedAt: now, UpdatedAt: now}
	if next, ok, err := spec.NextAfter(now); err != nil {
		return "", err
	} else if ok {
		at := next.At.UTC()
		sch.NextRunAt = &at
	}
	return id, a.Store.CreateJobDefinition(ctx, job, sch, in.SourceIDs)
}

func (a *Admin) defaultTimeout(kind domain.JobKind) time.Duration {
	if kind == domain.JobKindBackupLatest {
		return a.BackupTimeout
	}
	return a.CollectionTimeout
}

// List returns every job with its schedules and ordered sources.
func (a *Admin) List(ctx context.Context) ([]JobDefinition, error) {
	stored, err := a.Store.ListJobDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]JobDefinition, 0, len(stored))
	for _, d := range stored {
		result = append(result, JobDefinition{Job: d.Job, Schedules: d.Schedules, SourceIDs: d.SourceIDs})
	}
	return result, nil
}

// Enable turns a job on or off without touching its history.
func (a *Admin) Enable(ctx context.Context, id string, enabled bool) error {
	return a.Store.SetJobEnabled(ctx, id, enabled)
}

// Run queues a manual run of an enabled job. It is never coalesced and is
// executed by the dispatcher in order behind earlier runs of the same job.
func (a *Admin) Run(ctx context.Context, id string) (string, error) {
	return a.Store.EnqueueJobRun(ctx, id, a.now())
}
