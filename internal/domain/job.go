package domain

import "time"

// JobKind is what a configured unit of automatic work does.
type JobKind string

const (
	// JobKindCollection collects the job's selected sources.
	JobKindCollection JobKind = "COLLECTION"
	// JobKindBackupLatest backs up whatever stable staging state is current.
	JobKindBackupLatest JobKind = "BACKUP_LATEST"
	// JobKindVerify and JobKindRetention are accepted by the schema because the
	// architecture defines them. Nothing produces either yet: verification
	// cadence and retention are M8.
	JobKindVerify    JobKind = "VERIFY"
	JobKindRetention JobKind = "RETENTION"
)

// Job is one configured unit of automatic work.
//
// A job holds policy — what to run, how long to allow it, whether to retry —
// and never holds scheduling arithmetic. When it runs is the schedule's
// business, and a job with no schedule simply never fires.
type Job struct {
	ID   string
	Name string
	Kind JobKind
	// Enabled false stops the job firing without deleting its configuration or
	// its history.
	Enabled bool
	// BackupAfterCollection chains a backup onto a collection that published.
	BackupAfterCollection bool
	// PartialTriggersBackup decides whether a partial collection still chains a
	// backup. Backup eligibility independently excludes sources that did not
	// publish, so this governs whether to bother, not whether it is safe.
	PartialTriggersBackup bool
	// TimeoutSeconds bounds the whole run. It must be positive.
	TimeoutSeconds int64
	// RetryIntervalSeconds is the fixed wait between attempts.
	RetryIntervalSeconds int64
	// RetryDeadlineSeconds bounds retrying. Nil means no retry deadline, which
	// the architecture defines as a single attempt rather than as retrying
	// forever.
	RetryDeadlineSeconds *int64
	OptionsJSON          string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// RetriesAutomatically reports whether the job has an explicit retry deadline.
// Without one it gets a single attempt, per the architecture's retry strategy.
func (j Job) RetriesAutomatically() bool {
	return j.RetryDeadlineSeconds != nil && *j.RetryDeadlineSeconds > 0
}

// RunKind maps a job onto the run kind it produces.
func (k JobKind) RunKind() RunKind {
	switch k {
	case JobKindCollection:
		return RunKindCollection
	case JobKindBackupLatest:
		return RunKindBackupLatest
	case JobKindVerify:
		return RunKindVerify
	case JobKindRetention:
		return RunKindRetention
	default:
		return ""
	}
}

// Schedule is when a job is due: selected weekdays, one local time, a named
// timezone, and whether it is switched on.
//
// LastNominalRunAt and NextRunAt are bookkeeping, not inputs. They record what
// has already been claimed and cache what comes next; the arithmetic that
// produces them lives in the schedule package and depends only on the fields
// above them.
type Schedule struct {
	ID      string
	JobID   string
	Enabled bool
	// DaysOfWeekMask has bit N set for time.Weekday(N).
	DaysOfWeekMask uint8
	// LocalTime is "HH:MM" in 24-hour form.
	LocalTime string
	// Timezone is an IANA name.
	Timezone string
	// LastNominalRunAt is the nominal local due time most recently claimed,
	// stored as the canonical "YYYY-MM-DDTHH:MM" form rather than an instant:
	// it is an identity, and on a day when the clocks go back one identity has
	// two instants.
	LastNominalRunAt string
	// NextRunAt is the cached instant of the next occurrence.
	NextRunAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}
