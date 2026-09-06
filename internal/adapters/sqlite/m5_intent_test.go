package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func scheduleFixture(t *testing.T, store *Store) (domain.Job, domain.Schedule) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 9, 0, 0, 0, time.UTC)

	job := domain.Job{
		ID: "job-1", Name: "nightly", Kind: domain.JobKindCollection,
		Enabled: true, PartialTriggersBackup: true,
		TimeoutSeconds: 7200, RetryIntervalSeconds: 300,
		CreatedAt: at, UpdatedAt: at,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	due := time.Date(2026, time.September, 7, 0, 30, 0, 0, time.UTC)
	sched := domain.Schedule{
		ID: "sched-1", JobID: job.ID, Enabled: true,
		DaysOfWeekMask: 0b1111111, LocalTime: "02:30", Timezone: "Europe/Warsaw",
		NextRunAt: &due, CreatedAt: at, UpdatedAt: at,
	}
	if err := store.CreateSchedule(ctx, sched); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return job, sched
}

func intentFor(runID, key string) ScheduledIntent {
	at := time.Date(2026, time.September, 7, 0, 30, 0, 0, time.UTC)
	return ScheduledIntent{
		Run: domain.Run{
			ID: runID, Kind: domain.RunKindCollection,
			TriggerKind: domain.RunTriggerScheduled, Status: domain.RunStatusPending,
			RequestedAt: at, UpdatedAt: at,
		},
		IdempotencyKey: key,
		ScheduleID:     "sched-1",
		JobID:          "job-1",
		NominalDueAt:   "2026-09-07T02:30",
		NextRunAt:      at.AddDate(0, 0, 1),
	}
}

// The run and the schedule bookkeeping are one decision and must land together.
func TestClaimScheduledRunCommitsTheRunAndTheBookkeeping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduleFixture(t, store)

	claimed, err := store.ClaimScheduledRun(ctx, intentFor("run-1", "key-1"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatal("the first claim did not report itself as the winner")
	}

	run, err := store.Run(ctx, "run-1")
	if err != nil || run == nil {
		t.Fatalf("the claimed run was not stored: %v", err)
	}
	if run.Status != domain.RunStatusPending || run.TriggerKind != domain.RunTriggerScheduled {
		t.Fatalf("claimed run = %#v", run)
	}
	stored := scalar[string](t, store, `SELECT last_nominal_run_at FROM schedules WHERE id = 'sched-1'`)
	if stored != "2026-09-07T02:30" {
		t.Fatalf("schedule bookkeeping did not advance: %q", stored)
	}
	// The run carries its job, schedule and nominal identity for history.
	if linked := scalar[int](t, store, `SELECT COUNT(*) FROM runs
		WHERE id = 'run-1' AND job_id = 'job-1' AND schedule_id = 'sched-1'
		AND nominal_due_at = '2026-09-07T02:30' AND idempotency_key = 'key-1'`); linked != 1 {
		t.Fatal("the claimed run did not record what it was scheduled from")
	}
}

// Claiming the same occurrence twice must produce one run, and must still leave
// the schedule advanced so the due time is not retried forever.
func TestClaimScheduledRunIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduleFixture(t, store)

	if claimed, err := store.ClaimScheduledRun(ctx, intentFor("run-1", "key-1")); err != nil || !claimed {
		t.Fatalf("first claim: %v / %v", claimed, err)
	}
	// A different run id, the same occurrence: this is the second caller.
	claimed, err := store.ClaimScheduledRun(ctx, intentFor("run-2", "key-1"))
	if err != nil {
		t.Fatalf("second claim errored instead of reporting a duplicate: %v", err)
	}
	if claimed {
		t.Fatal("the second claim reported itself as the winner")
	}
	if count := scalar[int](t, store, `SELECT COUNT(*) FROM runs`); count != 1 {
		t.Fatalf("%d runs exist for one occurrence", count)
	}
	if _, err := store.Run(ctx, "run-2"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if run, _ := store.Run(ctx, "run-2"); run != nil {
		t.Fatal("the losing caller's run was stored")
	}
	// Advanced anyway: leaving it behind would make this schedule re-offer a
	// due time that has already been claimed, on every tick, forever.
	if stored := scalar[string](t, store,
		`SELECT last_nominal_run_at FROM schedules WHERE id = 'sched-1'`); stored != "2026-09-07T02:30" {
		t.Fatalf("a duplicate claim left the schedule unadvanced: %q", stored)
	}
}

// Concurrent callers must produce exactly one run and exactly one winner.
func TestClaimScheduledRunUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduleFixture(t, store)

	const callers = 8
	var wg sync.WaitGroup
	winners := make([]bool, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			winners[i], errs[i] = store.ClaimScheduledRun(ctx,
				intentFor("run-"+string(rune('a'+i)), "key-1"))
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i, err)
		}
		if winners[i] {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d callers claimed the same occurrence", won)
	}
	if count := scalar[int](t, store, `SELECT COUNT(*) FROM runs`); count != 1 {
		t.Fatalf("%d runs exist for one occurrence", count)
	}
}

// If the bookkeeping cannot advance, the run must not exist either. The reverse
// order is the dangerous one: a schedule advanced past a due time whose run was
// never created is work that silently never happens.
func TestClaimScheduledRunRollsBackWhenTheScheduleIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduleFixture(t, store)

	intent := intentFor("run-1", "key-1")
	intent.ScheduleID = "sched-missing"
	if _, err := store.ClaimScheduledRun(ctx, intent); err == nil {
		t.Fatal("a claim against a missing schedule succeeded")
	}
	if count := scalar[int](t, store, `SELECT COUNT(*) FROM runs`); count != 0 {
		t.Fatalf("the failed claim left %d run(s) behind", count)
	}
	// The real schedule is untouched, so its due time is still owed.
	if advanced := scalar[int](t, store,
		`SELECT COUNT(*) FROM schedules WHERE id = 'sched-1' AND last_nominal_run_at IS NULL`); advanced != 1 {
		t.Fatal("the failed claim advanced an unrelated schedule")
	}
}

// Both sweep queries must select every status the domain calls active. A status
// missing from either is a crashed run nothing will ever reconcile.
func TestActiveRunQueriesCoverEveryActiveStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	at := formatTime(time.Date(2026, time.September, 6, 9, 0, 0, 0, time.UTC))

	active := domain.ActiveRunStatuses()
	if len(active) < 7 {
		t.Fatalf("expected the widened active set, got %d", len(active))
	}
	for i, status := range active {
		id := "collection-" + string(rune('a'+i))
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
				requested_at, updated_at)
			VALUES (?, 'COLLECTION', 'SCHEDULED', ?, '', '{}', ?, ?)`,
			id, string(status), at, at); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
		repositoryID := "backup-" + string(rune('a'+i))
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
				requested_at, updated_at)
			VALUES (?, 'BACKUP_LATEST', 'SCHEDULED', ?, '', '{}', ?, ?)`,
			repositoryID, string(status), at, at); err != nil {
			t.Fatalf("seed backup %s: %v", status, err)
		}
	}
	// A terminal run must not be selected by either query.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, finished_at, updated_at)
		VALUES ('done', 'COLLECTION', 'MANUAL', 'SUCCEEDED', '', '{}', ?, ?, ?)`,
		at, at, at); err != nil {
		t.Fatalf("seed terminal run: %v", err)
	}

	all, err := store.ActiveRuns(ctx)
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if len(all) != len(active)*2 {
		t.Fatalf("ActiveRuns returned %d runs, want %d", len(all), len(active)*2)
	}
	seen := map[domain.RunStatus]int{}
	for _, run := range all {
		seen[run.Status]++
		if run.Status.Terminal() {
			t.Fatalf("ActiveRuns returned a terminal run: %s", run.Status)
		}
	}
	for _, status := range active {
		if seen[status] != 2 {
			t.Fatalf("ActiveRuns missed status %s", status)
		}
	}

	repository, err := store.ActiveRepositoryRuns(ctx)
	if err != nil {
		t.Fatalf("ActiveRepositoryRuns: %v", err)
	}
	if len(repository) != len(active) {
		t.Fatalf("ActiveRepositoryRuns returned %d runs, want %d", len(repository), len(active))
	}
	for _, run := range repository {
		if !run.Kind.TouchesRepository() {
			t.Fatalf("ActiveRepositoryRuns returned %s", run.Kind)
		}
	}
}

// A daemon with nothing configured must find nothing to do.
func TestNoJobsMeansNoActiveSchedules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduled, err := store.ActiveSchedules(ctx)
	if err != nil {
		t.Fatalf("ActiveSchedules: %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("a database with no jobs offered %d schedule(s)", len(scheduled))
	}
}

// Disabling either half takes the schedule out of consideration.
func TestActiveSchedulesRespectsBothEnabledFlags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scheduleFixture(t, store)

	if scheduled, _ := store.ActiveSchedules(ctx); len(scheduled) != 1 {
		t.Fatalf("the enabled fixture was not offered: %d", len(scheduled))
	}
	for _, disable := range []string{
		`UPDATE schedules SET enabled = 0 WHERE id = 'sched-1'`,
		`UPDATE jobs SET enabled = 0 WHERE id = 'job-1'`,
	} {
		if _, err := store.db.ExecContext(ctx, `UPDATE schedules SET enabled = 1 WHERE id = 'sched-1'`); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET enabled = 1 WHERE id = 'job-1'`); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := store.db.ExecContext(ctx, disable); err != nil {
			t.Fatalf("disable: %v", err)
		}
		scheduled, err := store.ActiveSchedules(ctx)
		if err != nil {
			t.Fatalf("ActiveSchedules: %v", err)
		}
		if len(scheduled) != 0 {
			t.Fatalf("%q left %d schedule(s) active", disable, len(scheduled))
		}
	}
}
