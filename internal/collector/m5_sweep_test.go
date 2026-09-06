package collector

import (
	"context"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

// The collection sweep must also cover every active status, and must still
// leave repository work to the recovery that owns it.
func TestCollectionRecoverySweepsEveryActiveStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, status := range domain.ActiveRunStatuses() {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, domain.PolicyArchiveThenSync)

			runID, err := domain.NewID()
			if err != nil {
				t.Fatalf("new id: %v", err)
			}
			if err := h.store.RecordRun(ctx, domain.Run{
				ID: runID, Kind: domain.RunKindCollection, SourceID: h.source.ID,
				TriggerKind: domain.RunTriggerScheduled, Status: status,
				RequestedAt: h.service.now(), UpdatedAt: h.service.now(),
			}); err != nil {
				t.Fatalf("record a run left %s: %v", status, err)
			}
			// A repository run in the same state must be left alone: backup
			// recovery needs its durability stage to decide what the media may
			// hold, and two sweeps resolving one run would race.
			backupRunID, err := domain.NewID()
			if err != nil {
				t.Fatalf("new id: %v", err)
			}
			if err := h.store.RecordRun(ctx, domain.Run{
				ID: backupRunID, Kind: domain.RunKindBackupLatest,
				TriggerKind: domain.RunTriggerScheduled, Status: status,
				RequestedAt: h.service.now(), UpdatedAt: h.service.now(),
			}); err != nil {
				t.Fatalf("record a backup run left %s: %v", status, err)
			}

			report, err := h.service.Recover(ctx)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if report.InterruptedRuns != 1 {
				t.Fatalf("a collection left %s was swept %d time(s)", status, report.InterruptedRuns)
			}

			swept, err := h.store.Run(ctx, runID)
			if err != nil || swept == nil {
				t.Fatalf("load swept run: %v", err)
			}
			if swept.Status != domain.RunStatusInterrupted {
				t.Fatalf("a collection left %s became %s, want INTERRUPTED", status, swept.Status)
			}
			if swept.Status.Successful() {
				t.Fatalf("a swept collection claimed success from %s", status)
			}

			untouched, err := h.store.Run(ctx, backupRunID)
			if err != nil || untouched == nil {
				t.Fatalf("load the repository run: %v", err)
			}
			if untouched.Status != status {
				t.Fatalf("the collection sweep resolved a repository run: %s", untouched.Status)
			}
		})
	}
}
