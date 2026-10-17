package backup

import (
	"context"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
)

// Startup recovery has to sweep a run left in *any* active status, not only the
// two an M4 run could reach.
//
// The state machine widened in slice 1 and the sweep queries widened with it.
// This is the behavioural half of that change: a crash in QUEUED, PREFLIGHT or
// RETRY_DELAY has to reach the same conclusion a crash in RUNNING does, because
// the process that died knew no more in one than the other. A status the sweep
// misses is a run nobody finishes and, when restic had started, a repository
// nobody marks suspect.
func TestRecoverSweepsEveryActiveStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, status := range domain.ActiveRunStatuses() {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.establishRepository()
			source := h.seedSource("documents", 1)

			// restic had started and named a snapshot nobody confirmed.
			runID := h.startStuckBackup(source, domain.StageSnapshotCreated, testSnapshotID)
			run := h.run(runID)
			run.Status = status
			if err := h.store.UpdateRun(ctx, run); err != nil {
				t.Fatalf("put the run into %s: %v", status, err)
			}

			report, err := h.service.Recover(ctx)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if report.InterruptedRuns != 1 {
				t.Fatalf("a run left %s was swept %d time(s)", status, report.InterruptedRuns)
			}
			swept := h.run(runID)
			if swept.Status != domain.RunStatusInterrupted {
				t.Fatalf("a run left %s became %s, want INTERRUPTED", status, swept.Status)
			}
			if swept.Status.Successful() {
				t.Fatalf("a swept run claimed success from %s", status)
			}
			// restic had run, so the repository cannot be trusted until an
			// explicit recovery looks at it.
			if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
				t.Fatalf("a run left %s produced repository health %s, want SUSPECT", status, health)
			}
			if backup := h.backupRun(runID); backup.VerifiedSnapshotID != "" ||
				backup.Durability == domain.DurabilityAdopted {
				t.Fatalf("the sweep adopted a snapshot from %s: %#v", status, backup)
			}
		})
	}
}

// M5 records an intent before choosing a disk or creating backup_runs metadata.
// This real persistence shape was absent from the earlier recovery tests.
func TestRecoverScheduledIntentWithoutBackupMetadata(t *testing.T) {
	t.Parallel()
	for _, status := range domain.ActiveRunStatuses() {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newHarness(t)
			h.establishRepository()
			job := domain.Job{ID: "scheduled-job", Name: "scheduled", Kind: domain.JobKindBackupLatest,
				Enabled: true, TimeoutSeconds: 3600, CreatedAt: h.now, UpdatedAt: h.now}
			if err := h.store.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			sched := domain.Schedule{ID: "scheduled-schedule", JobID: job.ID, Enabled: true,
				DaysOfWeekMask: 127, LocalTime: "12:00", Timezone: "UTC", CreatedAt: h.now, UpdatedAt: h.now}
			if err := h.store.CreateSchedule(ctx, sched); err != nil {
				t.Fatal(err)
			}
			run := domain.Run{ID: "scheduled-intent", Kind: domain.RunKindBackupLatest,
				TriggerKind: domain.RunTriggerScheduled, Status: status, RequestedAt: h.now, UpdatedAt: h.now}
			if claimed, err := h.store.ClaimScheduledRun(ctx, sqlite.ScheduledIntent{
				Run: run, ScheduleID: sched.ID, JobID: job.ID, IdempotencyKey: "scheduled-key",
				NominalDueAt: "2026-09-04T12:00", NextRunAt: h.now.Add(24 * time.Hour),
			}); err != nil || !claimed {
				t.Fatalf("claim: %v / %v", claimed, err)
			}
			report, err := h.service.Recover(ctx)
			if err != nil || report.InterruptedRuns != 1 || report.RepositoriesSuspected != 0 {
				t.Fatalf("recover metadata-free intent: %#v / %v", report, err)
			}
			if got := h.run(run.ID); got.Status != domain.RunStatusInterrupted || got.FinishedAt.IsZero() {
				t.Fatalf("unswept intent: %#v", got)
			}
			if metadata, err := h.store.BackupRun(ctx, run.ID); err != nil || metadata != nil {
				t.Fatalf("sweep invented backup metadata: %#v / %v", metadata, err)
			}
			if h.repositoryNow().Health != domain.RepositoryHealthy {
				t.Fatal("untouched repository became suspect")
			}
			active, err := h.store.ActiveRepositoryRuns(ctx)
			if err != nil || len(active) != 0 {
				t.Fatalf("active intents remain: %#v / %v", active, err)
			}
			events, err := h.store.ListEvents(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.RunID == run.ID && event.EventType == "repository.recovery_started" {
					found = true
				}
			}
			if !found {
				t.Fatal("interruption has no audit event")
			}
			if again, err := h.service.Recover(ctx); err != nil || again != (RecoveryReport{}) {
				t.Fatalf("repeated recovery: %#v / %v", again, err)
			}
		})
	}
}

// A run that never reached restic must not make the repository suspect, however
// it was left. Waiting for a disk that never arrived is not a reason to doubt
// what is on the media.
func TestRecoverDoesNotSuspectARepositoryResticNeverTouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, status := range []domain.RunStatus{
		domain.RunStatusWaitingForResource, domain.RunStatusQueued, domain.RunStatusPreflight,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.establishRepository()
			source := h.seedSource("documents", 1)

			runID := h.startStuckBackup(source, domain.StageRequested, "")
			run := h.run(runID)
			run.Status = status
			if err := h.store.UpdateRun(ctx, run); err != nil {
				t.Fatalf("put the run into %s: %v", status, err)
			}

			if _, err := h.service.Recover(ctx); err != nil {
				t.Fatalf("recover: %v", err)
			}
			if swept := h.run(runID); swept.Status != domain.RunStatusInterrupted {
				t.Fatalf("a run left %s became %s", status, swept.Status)
			}
			if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
				t.Fatalf("a run that never started restic left the repository %s", health)
			}
		})
	}
}
