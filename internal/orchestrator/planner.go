// Package orchestrator turns schedules into durable run intents and reconciles
// what a previous process left behind.
//
// This is the planning half only. It decides that work is due and records that
// decision durably; it does not start anything, hold any resource, or choose
// between competing operations. The dispatch loop, the worker pools and the
// resource coordinator are later slices, and keeping them out is deliberate: an
// intent that exists in the database but has not been started is safe, whereas
// an intent that is started before it is durable is not.
package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/schedule"
)

// Store is the persistence this planner needs.
type Store interface {
	ActiveSchedules(ctx context.Context) ([]sqlite.ScheduledJob, error)
	ClaimScheduledRun(ctx context.Context, intent sqlite.ScheduledIntent) (bool, error)
	SetScheduleNextRun(ctx context.Context, scheduleID string, nextRunAt, updatedAt time.Time) error
}

// maxCatchUpSteps bounds how far the planner will walk forward looking for the
// most recent missed occurrence. A daemon down for a year would otherwise make
// this loop proportional to the outage. Past the bound the planner stops
// searching and treats the schedule as simply overdue now, which errs towards
// running once rather than not at all.
const maxCatchUpSteps = 512

// Options configures a planner.
type Options struct {
	Store  Store
	Logger *slog.Logger
	// Now is injectable so tests can drive time rather than wait for it.
	Now func() time.Time
	// NewID generates run identifiers; injectable for deterministic tests.
	NewID func() (string, error)
}

// Planner creates durable intents from schedules.
type Planner struct {
	store  Store
	logger *slog.Logger
	now    func() time.Time
	newID  func() (string, error)
}

// New builds a planner.
func New(options Options) *Planner {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := options.NewID
	if newID == nil {
		newID = domain.NewID
	}
	return &Planner{store: options.Store, logger: logger, now: now, newID: newID}
}

// TickReport summarises one planning pass.
type TickReport struct {
	// Created counts intents this pass actually wrote.
	Created int
	// AlreadyClaimed counts due occurrences some other caller had already
	// written. They are not failures and not duplicates; they are the
	// idempotency key doing its job.
	AlreadyClaimed int
	// Skipped counts schedules that could not be evaluated, each with a reason
	// already logged.
	Skipped int
}

// Tick creates a durable intent for every schedule whose next occurrence has
// arrived.
//
// It starts nothing. What it produces is a PENDING run — the initial state of
// the architecture's run machine — and the schedule bookkeeping that goes with
// it, committed together.
//
// One thing it deliberately does not do is decide what to do about a job whose
// previous run is still going. Whether an overlapping occurrence should be
// skipped or queued is an unanswered product question, and guessing it here
// would bake an answer into durable state before anyone chose one. Until it is
// answered, this creates the intent and leaves the decision to the dispatcher
// that does not exist yet.
func (p *Planner) Tick(ctx context.Context) (TickReport, error) {
	var report TickReport
	now := p.now()

	scheduled, err := p.store.ActiveSchedules(ctx)
	if err != nil {
		return report, err
	}
	for _, pair := range scheduled {
		due, ok, err := p.dueOccurrence(pair, now)
		if err != nil {
			report.Skipped++
			p.logger.Warn("a schedule could not be evaluated",
				"schedule_id", pair.Schedule.ID, "job_id", pair.Job.ID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		claimed, err := p.claim(ctx, pair, due, now)
		if err != nil {
			return report, err
		}
		if claimed {
			report.Created++
		} else {
			report.AlreadyClaimed++
		}
	}
	return report, nil
}

// dueOccurrence decides whether a schedule has an occurrence to claim now, and
// which one.
//
// When the daemon has been down, several occurrences may have passed. The
// architecture allows at most one catch-up per job, and this picks the most
// recent of the missed occurrences rather than the oldest. The nominal due time
// is recorded on the run and shown in history, and a run labelled with a due
// time from three days ago would be claiming to be something it is not: it will
// collect or back up whatever is current when it executes, not what was current
// then.
func (p *Planner) dueOccurrence(pair sqlite.ScheduledJob, now time.Time) (schedule.Occurrence, bool, error) {
	spec := specOf(pair.Schedule)
	if err := spec.Validate(); err != nil {
		return schedule.Occurrence{}, false, err
	}

	// Without a cached next occurrence there is nothing owed: a schedule that
	// has never been evaluated is scheduled forward by Reconcile, not fired
	// retroactively for a due time that predates it being configured.
	if pair.Schedule.NextRunAt == nil {
		return schedule.Occurrence{}, false, nil
	}
	if pair.Schedule.NextRunAt.After(now) {
		return schedule.Occurrence{}, false, nil
	}

	// The cached instant is due. Walk forward to find the most recent occurrence
	// that is also due, so a long outage collapses to one run.
	cursor := pair.Schedule.NextRunAt.Add(-time.Minute)
	var latest schedule.Occurrence
	found := false
	for step := 0; step < maxCatchUpSteps; step++ {
		occurrence, ok, err := spec.NextAfter(cursor)
		if err != nil {
			return schedule.Occurrence{}, false, err
		}
		if !ok || occurrence.At.After(now) {
			break
		}
		latest, found = occurrence, true
		cursor = occurrence.At
	}
	return latest, found, nil
}

func (p *Planner) claim(ctx context.Context, pair sqlite.ScheduledJob,
	due schedule.Occurrence, now time.Time) (bool, error) {

	key, err := schedule.RunKey(pair.Job.ID, due.Nominal, domain.RunTriggerScheduled)
	if err != nil {
		return false, err
	}
	runID, err := p.newID()
	if err != nil {
		return false, err
	}
	next, ok, err := specOf(pair.Schedule).NextAfter(due.At)
	if err != nil {
		return false, err
	}
	if !ok {
		// Only a disabled schedule has no next occurrence, and ActiveSchedules
		// filtered those out, so reaching here means the schedule changed under
		// us. Leaving the bookkeeping where it is would re-fire the same due
		// time forever.
		return false, domain.NewError(domain.ErrClassInvariant, "schedule_has_no_next_occurrence",
			"a schedule that was due has no following occurrence")
	}

	kind := pair.Job.Kind.RunKind()
	if kind == "" {
		return false, domain.NewError(domain.ErrClassConfiguration, "job_kind_unknown",
			"this job has a kind ROTUP does not know how to run")
	}
	return p.store.ClaimScheduledRun(ctx, sqlite.ScheduledIntent{
		Run: domain.Run{
			ID:          runID,
			Kind:        kind,
			TriggerKind: domain.RunTriggerScheduled,
			Status:      domain.RunStatusPending,
			RequestedAt: now,
			UpdatedAt:   now,
		},
		IdempotencyKey: key,
		ScheduleID:     pair.Schedule.ID,
		JobID:          pair.Job.ID,
		NominalDueAt:   due.Nominal.String(),
		NextRunAt:      next.At,
	})
}

// ReconcileReport summarises what recomputing due times changed.
type ReconcileReport struct {
	// Scheduled counts schedules given a next occurrence they did not have or
	// whose cached value was wrong.
	Scheduled int
	// Unchanged counts schedules whose cached next occurrence was already right.
	Unchanged int
	// Skipped counts schedules that could not be evaluated.
	Skipped int
}

// Reconcile recomputes the cached next occurrence for every active schedule.
//
// It runs at startup, after the run sweep, and is the counterpart to it: the
// sweep decides what a dead process left unfinished, and this decides when each
// schedule is next owed. It claims nothing — a schedule whose time passed while
// the daemon was down is left due, and the next Tick collapses the whole outage
// into one intent.
func (p *Planner) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport
	now := p.now()

	scheduled, err := p.store.ActiveSchedules(ctx)
	if err != nil {
		return report, err
	}
	for _, pair := range scheduled {
		spec := specOf(pair.Schedule)
		if err := spec.Validate(); err != nil {
			report.Skipped++
			p.logger.Warn("a schedule could not be reconciled",
				"schedule_id", pair.Schedule.ID, "job_id", pair.Job.ID, "error", err)
			continue
		}
		// A schedule already sitting on a due occurrence keeps it. Recomputing
		// from now would move it into the future and silently discard the run
		// that was owed.
		if pair.Schedule.NextRunAt != nil && !pair.Schedule.NextRunAt.After(now) {
			report.Unchanged++
			continue
		}
		occurrence, ok, err := spec.NextAfter(now)
		if err != nil || !ok {
			report.Skipped++
			p.logger.Warn("a schedule has no next occurrence",
				"schedule_id", pair.Schedule.ID, "job_id", pair.Job.ID, "error", err)
			continue
		}
		if pair.Schedule.NextRunAt != nil && pair.Schedule.NextRunAt.Equal(occurrence.At) {
			report.Unchanged++
			continue
		}
		if err := p.store.SetScheduleNextRun(ctx, pair.Schedule.ID, occurrence.At, now); err != nil {
			return report, err
		}
		report.Scheduled++
	}
	return report, nil
}

func specOf(s domain.Schedule) schedule.Spec {
	return schedule.Spec{
		Days:      schedule.Weekdays(s.DaysOfWeekMask),
		LocalTime: s.LocalTime,
		Timezone:  s.Timezone,
		Enabled:   s.Enabled,
	}
}
