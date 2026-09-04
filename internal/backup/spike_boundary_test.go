package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

type beforeBackupEngine struct {
	ports.BackupEngine
	before func(ports.BackupRequest)
}

func (e beforeBackupEngine) Backup(ctx context.Context, request ports.BackupRequest) (ports.BackupResult, error) {
	e.before(request)
	return e.BackupEngine.Backup(ctx, request)
}

// restic can skip a missing root and still create a snapshot. The independent
// path-set proof must reject that snapshot now AND during later recovery.
func TestSpikeMissingStagingRootCannotEarnCoverage(t *testing.T) {
	h := newSpikeHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	other := h.seedSource("photos", 1)
	batch := h.seedRevisionBatch(source, true)
	current, err := h.layout.CurrentDir(source)
	if err != nil {
		t.Fatal(err)
	}
	h.service.engine = beforeBackupEngine{h.service.engine, func(ports.BackupRequest) {
		// Only a test-owned tree is moved, after capture but before restic reads.
		if err := os.Rename(current, current+"-removed"); err != nil {
			t.Fatal(err)
		}
	}}
	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID, other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.run(runID).Status.Successful() {
		t.Fatal("a snapshot omitting a selected root was accepted")
	}
	inspector, spec := h.spikeInspector(t)
	snapshots, err := inspector.Snapshots(context.Background(), spec)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("expected the real incomplete snapshot demonstrating this boundary: %v / %d", err, len(snapshots))
	}
	if _, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID); err != nil {
		t.Fatal(err)
	}
	if h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("recovery adopted a real snapshot omitting the recorded staging root")
	}
}

func TestSpikeCompletionCommitFailureIsRecoverableWithoutAnotherBackup(t *testing.T) {
	h := newSpikeHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	h.service.store = completionFailureStore{h.store}
	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("failed completion commit granted success")
	}
	h.service.store = h.store
	if _, err := h.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID); err != nil {
		t.Fatal(err)
	}
	if !h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 1 {
		t.Fatal("durable real snapshot was not adopted after the commit failure")
	}
	inspector, spec := h.spikeInspector(t)
	snapshots, err := inspector.Snapshots(context.Background(), spec)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("recovery must preserve exactly one snapshot: %v / %d", err, len(snapshots))
	}
}

func TestSpikeServiceCancellationAfterResticStarts(t *testing.T) {
	h := newSpikeHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	h.service.base = ctx
	observed := make(chan bool, 1)
	enteredBackup := false
	h.service.engine = beforeBackupEngine{h.service.engine, func(ports.BackupRequest) {
		enteredBackup = true
		go func() {
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				entries, err := os.ReadDir(filepath.Join(h.media.mountPoint, "rotup", "restic", "locks"))
				if err == nil && len(entries) > 0 {
					cancel() // The backup, not the preliminary inspection, holds a lock.
					observed <- true
					return
				}
				select {
				case <-ctx.Done():
					observed <- false
					return
				case <-ticker.C:
				}
			}
		}()
	}}
	defer cancel()
	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	cancel()
	if !enteredBackup {
		t.Fatalf("service did not reach its backup phase: %v", err)
	}
	started := <-observed // Join the watcher on every result.
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("could not observe the real restic process before cancellation")
	}
	if h.run(runID).Status != domain.RunStatusInterrupted || len(h.coverage(batch.ID)) != 0 || h.repositoryNow().Health.WritableForBackup() {
		t.Fatal("cancelled real service work did not remain interrupted, uncovered and suspect")
	}
	if h.backupRun(runID).Stage != domain.StageResticRunning {
		t.Fatal("the test did not interrupt the actual restic backup phase")
	}
	if h.media.busy(h.disk.ID) || h.barrier.BeingRead(source.ID) {
		t.Fatal("cancellation retained operation resources")
	}
}
