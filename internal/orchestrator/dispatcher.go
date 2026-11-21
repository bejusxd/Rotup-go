package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/coordinator"
	"github.com/bejusxd/rotup/internal/domain"
)

// Prepare acquires all in-memory resources, but does not start work. Work runs
// synchronously after the durable attempt claim. Release is always called.
type Prepare func(context.Context, sqlite.DispatchIntent, []string) (func(context.Context) error, func(), error)

// Dispatcher has one bounded worker. Planning runs independently so a long
// backup never loses schedule occurrences. SQLite owns the queue, not a channel.
type Dispatcher struct {
	Store   *sqlite.Store
	Prepare Prepare
	Logger  *slog.Logger
	Now     func() time.Time
	mu      sync.Mutex
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *Dispatcher) Tick(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	items, err := d.Store.DispatchQueue(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.dispatch(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) dispatch(ctx context.Context, item sqlite.DispatchIntent) error {
	now := d.now()
	if !item.RetryDeadlineAt.IsZero() && !now.Before(item.RetryDeadlineAt) {
		return d.fail(ctx, item, domain.NewError(domain.ErrClassTimeout, "retry_deadline_expired", "the job retry deadline has expired"))
	}
	if now.Before(item.RetryNotBefore) {
		return nil
	}
	ids, err := d.Store.DispatchSourceIDs(ctx, item)
	if err != nil {
		return err
	}
	if item.Job.TimeoutSeconds <= 0 || item.Job.TimeoutSeconds > 7*24*60*60 {
		return d.fail(ctx, item, domain.NewError(domain.ErrClassConfiguration, "job_timeout_invalid", "job timeout must be between one second and seven days"))
	}
	work, release, err := d.Prepare(ctx, item, ids)
	if err != nil {
		if coordinator.Busy(err) || errorCode(err) == "backup_media_waiting" || errorCode(err) == "backup_media_observation_unavailable" {
			run := item.Run
			run.Status = domain.RunStatusWaitingForResource
			run.Summary = "waiting for an available resource"
			if item.Run.Status == run.Status && item.WaitingReason == errorCode(err) {
				return nil
			}
			return d.Store.SetDispatchState(ctx, item, run, errorCode(err), time.Time{})
		}
		return d.fail(ctx, item, err)
	}
	defer release()
	claimed, err := d.Store.ClaimJobAttempt(ctx, item, now)
	if err != nil || !claimed {
		return err
	}
	if d.Logger != nil {
		d.Logger.Info("job attempt started", "run_id", item.Run.ID, "job_id", item.Job.ID, "attempt", item.AttemptCount+1)
	}
	attempt, cancel := context.WithTimeout(ctx, time.Duration(item.Job.TimeoutSeconds)*time.Second)
	workErr := work(attempt)
	contextErr := attempt.Err()
	cancel()
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cleanupCancel()
	current, err := d.Store.DispatchIntent(cleanup, item.Run.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("claimed job intent disappeared")
	}
	run := current.Run
	if run.Status.Successful() {
		return d.chain(cleanup, *current)
	}
	if errors.Is(contextErr, context.DeadlineExceeded) {
		run.Status = domain.RunStatusTimedOut
		run.ErrorClass = domain.ErrClassTimeout
		run.ErrorCode = "job_timeout"
		run.SafeError = "the job exceeded its configured timeout"
	} else if ctx.Err() != nil {
		run.Status = domain.RunStatusInterrupted
		run.ErrorClass = domain.ErrClassCancellation
		run.ErrorCode = "daemon_stopping"
		run.SafeError = "the daemon stopped during this job"
	} else if workErr != nil || !run.Status.Terminal() {
		run.Status = domain.RunStatusFailed
		setFailure(&run, workErr)
	}
	if !run.Status.Terminal() {
		return errors.New("job executor did not produce a terminal result")
	}
	run.FinishedAt = d.now()
	if run.SafeError != "" {
		run.Summary = run.SafeError
	}
	if err := d.Store.SetDispatchState(cleanup, *current, run, "", time.Time{}); err != nil {
		return err
	}
	current.Run = run
	if run.Status == domain.RunStatusPartial {
		return d.chain(cleanup, *current)
	}
	if (run.Status == domain.RunStatusFailed || run.Status == domain.RunStatusTimedOut) &&
		(run.ErrorClass == domain.ErrClassUnavailable || run.ErrorClass == domain.ErrClassTimeout) && current.Job.RetriesAutomatically() {
		safe, err := d.Store.JobRetrySafe(cleanup, run.ID)
		if err != nil {
			return err
		}
		next := d.now().Add(time.Duration(current.Job.RetryIntervalSeconds) * time.Second)
		if safe && next.Before(current.RetryDeadlineAt) {
			run.Status = domain.RunStatusRetryDelay
			run.FinishedAt = time.Time{}
			run.Summary = "waiting for the next retry"
			return d.Store.SetDispatchState(cleanup, *current, run, "", next)
		}
	}
	return nil
}

func (d *Dispatcher) fail(ctx context.Context, item sqlite.DispatchIntent, err error) error {
	run := item.Run
	run.Status = domain.RunStatusFailed
	run.FinishedAt = d.now()
	setFailure(&run, err)
	run.Summary = run.SafeError
	return d.Store.SetDispatchState(ctx, item, run, "", time.Time{})
}

func setFailure(run *domain.Run, err error) {
	run.ErrorClass, run.ErrorCode, run.SafeError = domain.ErrClassInternal, "job_outcome_unconfirmed", "the job did not commit a confirmed outcome"
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		run.ErrorClass, run.ErrorCode, run.SafeError = typed.Class, typed.Code, typed.SafeMessage()
	}
}

func errorCode(err error) string {
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		return typed.Code
	}
	return "internal_error"
}

func (d *Dispatcher) chain(ctx context.Context, item sqlite.DispatchIntent) error {
	if item.Run.Kind != domain.RunKindCollection || !item.Job.BackupAfterCollection {
		return nil
	}
	if !item.Run.Status.Successful() && !(item.Run.Status == domain.RunStatusPartial && item.Job.PartialTriggersBackup) {
		return nil
	}
	return d.Store.EnqueueChainedBackup(ctx, item.Run.ID, d.now())
}
