package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
)

// harness wires the planner over a real store on a temporary database, with a
// clock the test drives. Nothing here starts work: the planner only writes
// intents, so a test can advance time as fast as it likes.
type harness struct {
	t       *testing.T
	store   *sqlite.Store
	planner *Planner
	now     time.Time
	ids     int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "rotup.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h := &harness{t: t, store: store, now: time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)}
	h.planner = New(Options{
		Store:  store,
		Logger: logger,
		Now:    func() time.Time { return h.now },
		NewID: func() (string, error) {
			h.ids++
			return "run-" + strconv.Itoa(h.ids), nil
		},
	})
	return h
}

// seeded describes the fixture a test wants. The zero value is one enabled
// daily job at 02:30 Europe/Warsaw with no cached next occurrence, which is the
// state a freshly configured schedule is in.
type seeded struct {
	kind             domain.JobKind
	jobDisabled      bool
	scheduleOff      bool
	timezoneOverride string
}

func (h *harness) seed(fixture seeded) {
	h.t.Helper()
	ctx := context.Background()
	kind := fixture.kind
	if kind == "" {
		kind = domain.JobKindCollection
	}
	zone := fixture.timezoneOverride
	if zone == "" {
		zone = "Europe/Warsaw"
	}
	job := domain.Job{
		ID: "job-1", Name: "nightly", Kind: kind, Enabled: !fixture.jobDisabled,
		PartialTriggersBackup: true, TimeoutSeconds: 7200,
		RetryIntervalSeconds: 300, CreatedAt: h.now, UpdatedAt: h.now,
	}
	if err := h.store.CreateJob(ctx, job); err != nil {
		h.t.Fatalf("create job: %v", err)
	}
	sched := domain.Schedule{
		ID: "sched-1", JobID: job.ID, Enabled: !fixture.scheduleOff,
		DaysOfWeekMask: 0b1111111, LocalTime: "02:30", Timezone: zone,
		CreatedAt: h.now, UpdatedAt: h.now,
	}
	if err := h.store.CreateSchedule(ctx, sched); err != nil {
		h.t.Fatalf("create schedule: %v", err)
	}
}

func (h *harness) schedule() domain.Schedule {
	h.t.Helper()
	loaded, err := h.store.Schedule(context.Background(), "sched-1")
	if err != nil || loaded == nil {
		h.t.Fatalf("load schedule: %v", err)
	}
	return *loaded
}

func (h *harness) runs() []domain.Run {
	h.t.Helper()
	runs, err := h.store.ListRuns(context.Background(), 100)
	if err != nil {
		h.t.Fatalf("list runs: %v", err)
	}
	return runs
}

func (h *harness) tick() TickReport {
	h.t.Helper()
	report, err := h.planner.Tick(context.Background())
	if err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	return report
}

func (h *harness) reconcile() ReconcileReport {
	h.t.Helper()
	report, err := h.planner.Reconcile(context.Background())
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return report
}

// A schedule with nothing cached is scheduled forward, not fired for a due time
// that predates it being configured.
func TestReconcileSchedulesForwardWithoutClaiming(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seed(seeded{})

	if report := h.reconcile(); report.Scheduled != 1 {
		t.Fatalf("reconcile scheduled %d, want 1", report.Scheduled)
	}
	next := h.schedule().NextRunAt
	if next == nil {
		t.Fatal("reconcile left the schedule with no next occurrence")
	}
	if !next.After(h.now) {
		t.Fatalf("next occurrence %s is not in the future", next)
	}
	if len(h.runs()) != 0 {
		t.Fatal("reconcile created a run")
	}
	// Running it again changes nothing.
	if report := h.reconcile(); report.Unchanged != 1 || report.Scheduled != 0 {
		t.Fatalf("second reconcile = %#v", report)
	}
}

// The due boundary: before, exactly at, and after.
func TestTickFiresOnlyOnceTheOccurrenceHasArrived(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seed(seeded{})
	h.reconcile()
	due := *h.schedule().NextRunAt

	h.now = due.Add(-time.Minute)
	if report := h.tick(); report.Created != 0 {
		t.Fatalf("a tick before the due time created %d run(s)", report.Created)
	}

	h.now = due
	if report := h.tick(); report.Created != 1 {
		t.Fatalf("a tick exactly at the due time created %d run(s)", report.Created)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("%d runs after the first tick", len(runs))
	}
	if runs[0].Status != domain.RunStatusPending {
		t.Fatalf("a scheduled intent was created as %s, want PENDING", runs[0].Status)
	}
	if runs[0].TriggerKind != domain.RunTriggerScheduled || runs[0].Kind != domain.RunKindCollection {
		t.Fatalf("intent = %#v", runs[0])
	}

	// The schedule moved on, so ticking again claims nothing.
	if report := h.tick(); report.Created != 0 || report.AlreadyClaimed != 0 {
		t.Fatalf("a repeated tick = %#v", report)
	}
	if len(h.runs()) != 1 {
		t.Fatal("a repeated tick created a second run")
	}
	if next := h.schedule().NextRunAt; next == nil || !next.After(h.now) {
		t.Fatalf("the schedule did not advance past the claimed occurrence: %v", next)
	}
}

// Ticking repeatedly within one due window must not accumulate runs.
func TestRepeatedTicksClaimOneRunPerOccurrence(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seed(seeded{kind: domain.JobKindBackupLatest})
	h.reconcile()
	h.now = h.schedule().NextRunAt.Add(time.Minute)

	for i := 0; i < 5; i++ {
		h.tick()
	}
	if runs := h.runs(); len(runs) != 1 {
		t.Fatalf("five ticks in one window produced %d runs", len(runs))
	}
}

// A daemon that was down for a week owes one run, not seven.
func TestDowntimeCollapsesToASingleCatchUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seed(seeded{})
	h.reconcile()
	firstMissed := *h.schedule().NextRunAt

	// Come back a week later.
	h.now = firstMissed.AddDate(0, 0, 7).Add(2 * time.Hour)

	// Reconcile must leave the overdue schedule alone rather than pushing it
	// into the future, or the owed run would be silently discarded.
	if report := h.reconcile(); report.Unchanged != 1 || report.Scheduled != 0 {
		t.Fatalf("reconcile after downtime = %#v", report)
	}
	if next := h.schedule().NextRunAt; next == nil || !next.Equal(firstMissed) {
		t.Fatalf("reconcile moved an overdue schedule: %v", next)
	}

	if report := h.tick(); report.Created != 1 {
		t.Fatalf("catch-up created %d run(s), want exactly 1", report.Created)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("a week of downtime produced %d runs", len(runs))
	}

	// The one run stands for the most recent missed occurrence, not the oldest:
	// it will collect whatever is current when it executes, so labelling it with
	// a week-old due time would misdescribe it.
	claimed := h.schedule().LastNominalRunAt
	wantNominal := h.now.In(warsaw(t)).Format("2006-01-02") + "T02:30"
	if claimed != wantNominal {
		t.Fatalf("catch-up claimed %s, want the most recent missed occurrence %s",
			claimed, wantNominal)
	}
	oldest := firstMissed.In(warsaw(t)).Format("2006-01-02") + "T02:30"
	if claimed == oldest {
		t.Fatalf("catch-up claimed the oldest missed occurrence %s", oldest)
	}

	// And the schedule is now ahead of the catch-up, so the next tick is quiet.
	if report := h.tick(); report.Created != 0 {
		t.Fatalf("a second tick after catch-up created %d run(s)", report.Created)
	}
}

// Neither half being disabled may produce work.
func TestDisabledJobsAndSchedulesAreInert(t *testing.T) {
	t.Parallel()

	for name, fixture := range map[string]seeded{
		"schedule off": {scheduleOff: true},
		"job off":      {jobDisabled: true},
	} {
		h := newHarness(t)
		h.seed(fixture)

		// Nothing is even offered for planning, so neither pass can act.
		if report := h.reconcile(); report != (ReconcileReport{}) {
			t.Fatalf("%s: reconcile = %#v", name, report)
		}
		h.now = h.now.AddDate(0, 0, 2)
		if report := h.tick(); report.Created != 0 {
			t.Fatalf("%s still produced %d run(s)", name, report.Created)
		}
		if len(h.runs()) != 0 {
			t.Fatalf("%s still produced runs", name)
		}
		if h.schedule().NextRunAt != nil {
			t.Fatalf("%s was given a next occurrence", name)
		}
	}
}

// With nothing configured, planning is a no-op in both directions.
func TestNoConfigurationIsInert(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if report := h.reconcile(); report != (ReconcileReport{}) {
		t.Fatalf("reconcile with no jobs = %#v", report)
	}
	if report := h.tick(); report != (TickReport{}) {
		t.Fatalf("tick with no jobs = %#v", report)
	}
	if len(h.runs()) != 0 {
		t.Fatal("an unconfigured daemon created a run")
	}
}

// An unevaluatable schedule is reported and stepped over, not fatal: one broken
// schedule must not stop every other job in the daemon.
func TestABrokenScheduleIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seed(seeded{timezoneOverride: "Mars/Olympus"})

	if report := h.reconcile(); report.Skipped != 1 || report.Scheduled != 0 {
		t.Fatalf("reconcile = %#v", report)
	}
	if report := h.tick(); report.Created != 0 {
		t.Fatalf("a broken schedule produced %d run(s)", report.Created)
	}
}

func warsaw(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Skipf("Europe/Warsaw unavailable: %v", err)
	}
	return location
}
