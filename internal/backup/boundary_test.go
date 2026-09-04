package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// Fail just the local commit after the external side effect, without replacing
// the actual SQLite read-set, history or coverage implementation.
type completionFailureStore struct{ Store }

func (s completionFailureStore) CompleteBackupRun(context.Context, domain.Run, domain.BackupRun, []domain.RevisionCoverage) error {
	return errors.New("injected completion transaction failure")
}

func pendingDurableBackup(t *testing.T, h *harness) (string, domain.RevisionBatch) {
	t.Helper()
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	h.media.unmountErr = domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorSyncFailed, "injected sync failure")
	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("failed durability already recorded success or coverage")
	}
	return runID, batch
}

func TestRecoveryWaitsForDurabilityBeforeCoverage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID, batch := pendingDurableBackup(t, h)
	recovery, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.run(recovery).Status.Successful() || h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("recovery with failed sync/unmount recorded a successful backup or coverage")
	}
	h.media.unmountErr = nil
	if _, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID); err != nil {
		t.Fatal(err)
	}
	if !h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 1 {
		t.Fatal("subsequent durable recovery did not adopt the verified snapshot")
	}
}

func TestRecoveryCompletionFailureCannotReturnHealthy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runID, batch := pendingDurableBackup(t, h)
	h.media.unmountErr = nil
	h.service.store = completionFailureStore{h.store}
	recovery, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.run(recovery).Status.Successful() || h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a failed recovery commit was hidden by a successful VERIFY run")
	}
	if h.repositoryNow().Health.WritableForBackup() {
		t.Fatal("a failed recovery commit returned the repository to writable")
	}
}

func TestRecoveryDeclinesSnapshotsWithoutCleanCompletionEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"unknown exit", "known read errors", "missing run tag", "ambiguous run tag"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.establishRepository()
			source := h.seedSource("documents", 1)
			candidate := ""
			stage := domain.StageResticRunning
			if name == "missing run tag" || name == "ambiguous run tag" {
				candidate, stage = testSnapshotID, domain.StageSnapshotCreated
			}
			runID := h.startStuckBackup(source, stage, candidate)
			tags := []string{"rotup-run:" + runID}
			if name == "missing run tag" {
				tags = nil
			}
			h.engine.snapshots = []ports.Snapshot{{ID: testSnapshotID, Tags: tags}}
			if name == "ambiguous run tag" {
				h.engine.snapshots = append(h.engine.snapshots, ports.Snapshot{ID: testResticID, Tags: tags})
			}
			if name == "known read errors" {
				if err := h.store.RecordCandidateSnapshot(context.Background(), runID, testSnapshotID, domain.BackupRun{EngineErrors: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.service.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID); err != nil {
				t.Fatal(err)
			}
			if h.run(runID).Status.Successful() || h.backupRun(runID).Stage == domain.StageDurableSuccess {
				t.Fatal("a snapshot without unique provenance and clean completion evidence was adopted")
			}
		})
	}
}

func TestStartupRecoveryFindsPendingBackupEvenWhenRunAlreadyTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	runID := h.startStuckBackup(source, domain.StageResticRunning, "")
	run := h.run(runID)
	run.Status = domain.RunStatusInterrupted
	if err := h.store.UpdateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.repositoryNow().Health.WritableForBackup() {
		t.Fatal("a crash between interrupting the run and suspecting the repository was missed")
	}
}

func TestBackupFailureAfterCancellationPersistsAndCleansUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.base = ctx
	h.engine.onBackup = cancel
	h.engine.backupErr = domain.NewError(domain.ErrClassCancellation, domain.RepositoryErrorInterrupted, "cancelled")
	h.media.onUnmount = func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited the cancelled operation context")
		}
	}
	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.run(runID).Status != domain.RunStatusInterrupted || h.repositoryNow().Health.WritableForBackup() {
		t.Fatal("cancellation prevented terminal failure and suspect state from being persisted")
	}
	if h.media.busy(h.disk.ID) || h.barrier.BeingRead(source.ID) {
		t.Fatal("cancelled work retained a lease or staging barrier")
	}
}

func TestBusySetupCannotReplaceRepositoryPassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.registerDisk()
	repository, err := h.service.resolveSetupTarget(context.Background(), h.disk, SetupInitialize)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.storeRepositoryPassword(context.Background(), *repository, testPassword); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.RepositoryCredentialRef(context.Background(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := h.media.Acquire(context.Background(), h.disk.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: "a-different-password", EscrowAcknowledged: true,
	}); err == nil {
		t.Fatal("a concurrent setup was accepted")
	}
	after, err := h.store.RepositoryCredentialRef(context.Background(), repository.ID)
	if err != nil || before.ProviderLocator != after.ProviderLocator {
		t.Fatal("a refused setup changed the active password reference")
	}
}

func TestFailedInitializationWithoutIdentityIsSuspect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.registerDisk()
	h.engine.initErr = errEngineFailed
	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := h.store.RepositoryForDisk(context.Background(), h.disk.ID)
	if err != nil || repository == nil || repository.Health != domain.RepositorySuspect || h.run(runID).Status.Successful() {
		t.Fatal("failed init was treated as an untouched or usable repository")
	}
}
