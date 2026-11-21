package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
)

func queueFixture(t *testing.T) *harness {
	h := newHarness(t)
	h.seed(seeded{})
	h.reconcile()
	h.now = *h.schedule().NextRunAt
	h.tick()
	return h
}

func TestDispatcherWaitsWithoutAttemptsThenExecutesSameIntent(t *testing.T) {
	h := queueFixture(t)
	ctx := context.Background()
	run := h.runs()[0]
	busy := true
	released := 0
	executed := 0
	d := Dispatcher{Store: h.store, Now: func() time.Time { return h.now }, Prepare: func(ctx context.Context, item sqlite.DispatchIntent, ids []string) (func(context.Context) error, func(), error) {
		if busy {
			return nil, nil, domain.NewError(domain.ErrClassUnavailable, "disk_busy", "busy")
		}
		return func(ctx context.Context) error {
			executed++
			saved, err := h.store.DispatchIntent(ctx, item.Run.ID)
			if err != nil {
				return err
			}
			if saved.AttemptCount != 1 {
				t.Fatal("work started before claim")
			}
			r := saved.Run
			r.Status = domain.RunStatusSucceeded
			r.FinishedAt = h.now
			return h.store.UpdateRun(ctx, r)
		}, func() { released++ }, nil
	}}
	for i := 0; i < 3; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	item, err := h.store.DispatchIntent(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Run.Status != domain.RunStatusWaitingForResource || item.AttemptCount != 0 || executed != 0 {
		t.Fatalf("waiting consumed work: %+v", item)
	}
	busy = false
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	item, _ = h.store.DispatchIntent(ctx, run.ID)
	if item.Run.Status != domain.RunStatusSucceeded || item.AttemptCount != 1 || executed != 1 || released != 1 {
		t.Fatalf("completion=%+v executed=%d released=%d", item, executed, released)
	}
	if len(h.runs()) != 1 {
		t.Fatal("executor created another run")
	}
}

func TestDispatcherCancellationReleasesWorkerAndDoesNotGrantSuccess(t *testing.T) {
	h := queueFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := false
	d := Dispatcher{Store: h.store, Now: func() time.Time { return h.now }, Prepare: func(context.Context, sqlite.DispatchIntent, []string) (func(context.Context) error, func(), error) {
		return func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }, func() { released = true }, nil
	}}
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	item, err := h.store.DispatchIntent(context.Background(), h.runs()[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Run.Status != domain.RunStatusInterrupted || !released {
		t.Fatalf("cancellation=%+v released=%v", item, released)
	}
}

func TestDispatcherRetriesOnlyWithExplicitPolicyAndDeadline(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "one attempt", true: "bounded retry"}[retry], func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			deadline := int64(600)
			job := domain.Job{ID: "job", Name: "job", Kind: domain.JobKindCollection, Enabled: true, TimeoutSeconds: 30, RetryIntervalSeconds: 60, CreatedAt: h.now, UpdatedAt: h.now}
			if retry {
				job.RetryDeadlineSeconds = &deadline
			}
			if err := h.store.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			schedule := domain.Schedule{ID: "schedule", JobID: job.ID, Enabled: true, DaysOfWeekMask: 127, LocalTime: "12:00", Timezone: "UTC", CreatedAt: h.now, UpdatedAt: h.now}
			if err := h.store.CreateSchedule(ctx, schedule); err != nil {
				t.Fatal(err)
			}
			intent := sqlite.ScheduledIntent{Run: domain.Run{ID: "run", Kind: domain.RunKindCollection, TriggerKind: domain.RunTriggerScheduled, Status: domain.RunStatusPending, RequestedAt: h.now, UpdatedAt: h.now}, JobID: job.ID, ScheduleID: schedule.ID, IdempotencyKey: "run", NominalDueAt: "2026-09-06T12:00", NextRunAt: h.now.AddDate(0, 0, 1)}
			if _, err := h.store.ClaimScheduledRun(ctx, intent); err != nil {
				t.Fatal(err)
			}
			calls := 0
			d := Dispatcher{Store: h.store, Now: func() time.Time { return h.now }, Prepare: func(_ context.Context, item sqlite.DispatchIntent, _ []string) (func(context.Context) error, func(), error) {
				return func(ctx context.Context) error {
					calls++
					run, err := h.store.Run(ctx, item.Run.ID)
					if err != nil {
						return err
					}
					run.Status = domain.RunStatusFailed
					run.ErrorClass = domain.ErrClassUnavailable
					run.ErrorCode = "share_unavailable"
					run.FinishedAt = h.now
					return h.store.UpdateRun(ctx, *run)
				}, func() {}, nil
			}}
			if err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			current, _ := h.store.DispatchIntent(ctx, "run")
			want := domain.RunStatusFailed
			if retry {
				want = domain.RunStatusRetryDelay
			}
			if current.Run.Status != want {
				t.Fatalf("status=%s", current.Run.Status)
			}
			if err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("early retry")
			}
			h.now = h.now.Add(time.Minute)
			if err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if retry && calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
			h.now = h.now.Add(time.Hour)
			if err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			current, _ = h.store.DispatchIntent(ctx, "run")
			if current.Run.Status != domain.RunStatusFailed {
				t.Fatal("retry deadline not terminal")
			}
		})
	}
}
