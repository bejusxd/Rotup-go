package backup

import (
	"context"
	"testing"

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
