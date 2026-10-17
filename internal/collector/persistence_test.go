package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

type failedCollectionStore struct {
	Store
	failResult   bool
	failRunning  bool
	failTerminal bool
	failRecovery bool
}

func (s *failedCollectionStore) SetSourceStagingStatus(ctx context.Context, id string, status domain.StagingStatus) error {
	if s.failRecovery && status == domain.StagingRecoveryRequired {
		return errors.New("injected recovery-state transaction failure")
	}
	return s.Store.SetSourceStagingStatus(ctx, id, status)
}

func (s *failedCollectionStore) SaveSourceResult(ctx context.Context, result domain.SourceResult, source *domain.Source) error {
	if s.failResult {
		return errors.New("injected result transaction failure")
	}
	return s.Store.SaveSourceResult(ctx, result, source)
}

func (s *failedCollectionStore) UpdateRun(ctx context.Context, run domain.Run) error {
	if (s.failRunning && run.Status == domain.RunStatusRunning) || (s.failTerminal && run.Status.Terminal()) {
		return errors.New("injected run transaction failure")
	}
	return s.Store.UpdateRun(ctx, run)
}

func TestCollectionResultCommitFailureCannotReportSuccess(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "new bytes")
	h.service.store = &failedCollectionStore{Store: h.store, failResult: true}
	run, source := h.collect()
	if run.Status != domain.RunStatusFailed || run.ErrorCode != "collection_result_persistence_failed" {
		t.Fatalf("uncommitted collection result reported as %s / %s", run.Status, run.ErrorCode)
	}
	if source.StagingStatus != domain.StagingRecoveryRequired || source.StagingStateVersion != 0 {
		t.Fatalf("uncommitted state advertised as published: %+v", source)
	}
	if !h.service.workSuggestsMutation(source.ID) {
		t.Fatal("recovery evidence lost")
	}
	if h.service.Busy(source.ID) {
		t.Fatal("failed persistence leaked resources")
	}
	assertFiles(t, "uncommitted bytes retained", h.currentFiles(), map[string]string{"a.txt": "new bytes"})
	result, err := h.store.SourceResult(context.Background(), run.ID, source.ID)
	if err != nil || result != nil {
		t.Fatalf("rolled-back result should be absent: %v, %v", result, err)
	}
}

func TestCollectionMustRecordRunningBeforeMutating(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "must not be copied")
	h.service.store = &failedCollectionStore{Store: h.store, failRunning: true}
	run, source := h.collect()
	if run.Status != domain.RunStatusPending {
		t.Fatalf("status = %s", run.Status)
	}
	if source.StagingStateVersion != 0 {
		t.Fatal("started collection without durable running state")
	}
	if len(h.currentFiles()) != 0 {
		t.Fatal("mutated staging after failed start transaction")
	}
	if h.service.Busy(source.ID) {
		t.Fatal("failed start leaked resources")
	}
}

func TestFailedTerminalCommitRemainsRecoverableNotSuccessful(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.service.store = &failedCollectionStore{Store: h.store, failTerminal: true}
	run, source := h.collect()
	if run.Status != domain.RunStatusRunning {
		t.Fatalf("uncommitted success: %s", run.Status)
	}
	// The publication committed; the aggregate run did not. A restart must not
	// infer success from the source row and thereby trigger a chained backup.
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("publication not committed: %s", source.StagingStatus)
	}
	h.service.store = h.store
	if _, err := h.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ = h.reload(run.ID)
	if run.Status != domain.RunStatusInterrupted {
		t.Fatalf("recovery status = %s", run.Status)
	}
}

func TestResultFailureBeforeMutationPreservesLastGoodStaging(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "last good copy")
	first, original := h.collect()
	if !first.Status.Successful() {
		t.Fatal(first.SafeError)
	}
	h.engine.connectErr = errors.New("share unavailable")
	h.service.store = &failedCollectionStore{Store: h.store, failResult: true}
	run, source := h.collect()
	if run.Status.Successful() || source.StagingStatus != domain.StagingStable || source.StagingStateVersion != original.StagingStateVersion {
		t.Fatalf("untouched staging changed after preflight failure: run=%s source=%+v", run.Status, source)
	}
	assertFiles(t, "last good staging", h.currentFiles(), map[string]string{"a.txt": "last good copy"})
}

func TestTotalFinalizationWriteFailureIsSweptConservatively(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "retained")
	h.service.store = &failedCollectionStore{Store: h.store, failResult: true, failTerminal: true, failRecovery: true}
	run, source := h.collect()
	if run.Status != domain.RunStatusRunning || source.StagingStatus != domain.StagingUpdating {
		t.Fatalf("uncommitted finalization escaped fail-closed state: %s / %s", run.Status, source.StagingStatus)
	}
	h.service.store = h.store
	if _, err := h.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, source = h.reload(run.ID)
	if run.Status != domain.RunStatusInterrupted || source.StagingStatus != domain.StagingRecoveryRequired {
		t.Fatalf("recovery = %s / %s", run.Status, source.StagingStatus)
	}
	assertFiles(t, "retained bytes", h.currentFiles(), map[string]string{"a.txt": "retained"})
}
