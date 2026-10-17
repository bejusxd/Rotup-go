package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/coordinator"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/staging"
)

func TestCoordinatorAndManualOperationsShareSourceLock(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	ctx := context.Background()
	barrier := staging.NewBarrier()
	h.service.barrier = barrier
	c := coordinator.New(barrier, h.service, nil)
	release, err := c.TryCollection(ctx, h.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := h.service.StartConnectionTest(ctx, h.source.ID); !coordinator.Busy(err) {
		t.Fatalf("manual connection test bypassed scheduled source lock: %v", err)
	}
	release()
	manualRelease, err := h.service.TryAcquireSource(h.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer manualRelease()
	if _, err := c.TryCollection(ctx, h.source.ID); !coordinator.Busy(err) {
		t.Fatalf("coordinator bypassed manual source lock: %v", err)
	}
	if barrier.Mutating(h.source.ID) {
		t.Fatal("failed acquisition leaked the staging barrier")
	}
	manualRelease()
	release, err = c.TryCollection(ctx, h.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// harness wires the collector against a real SQLite store, a real staging
// layout on a temporary directory, and the filesystem-backed fake engine.
type harness struct {
	t       *testing.T
	store   *sqlite.Store
	layout  *staging.Layout
	engine  *fakeEngine
	service *Service
	admin   *Admin
	source  domain.Source
	secrets *fakeSecretStore
	sourceD string
}

func newHarness(t *testing.T, policy domain.ReconciliationPolicy) *harness {
	t.Helper()

	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := sqlite.Open(context.Background(), filepath.Join(root, "rotup.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	layout, err := staging.NewLayout(filepath.Join(root, "staging"))
	if err != nil {
		t.Fatalf("staging layout: %v", err)
	}
	sourceDir := filepath.Join(root, "share")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("create fake share: %v", err)
	}

	engine := &fakeEngine{sourceDir: sourceDir}
	secretStore := newFakeSecretStore()
	admin := NewAdmin(store, secretStore, layout, logger)
	service := New(context.Background(), Options{
		Store:     store,
		Engine:    engine,
		Layout:    layout,
		Logger:    logger,
		RunInline: true,
	})

	id, err := admin.Create(context.Background(), SourceInput{
		Name:                 "documents",
		Host:                 "fileserver.example",
		ShareName:            "docs",
		Username:             "reader",
		StagingRelativePath:  "documents",
		Enabled:              true,
		ReconciliationPolicy: policy,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	source, err := store.Source(context.Background(), id)
	if err != nil || source == nil {
		t.Fatalf("load source: %v", err)
	}

	return &harness{
		t: t, store: store, layout: layout, engine: engine,
		service: service, admin: admin, source: *source,
		secrets: secretStore, sourceD: sourceDir,
	}
}

func (h *harness) writeSource(relative, content string) {
	h.t.Helper()
	path := filepath.Join(h.sourceD, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatalf("create source directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatalf("write source file: %v", err)
	}
}

func (h *harness) removeSource(relative string) {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.sourceD, filepath.FromSlash(relative))); err != nil {
		h.t.Fatalf("remove source file: %v", err)
	}
}

func (h *harness) currentDir() string {
	h.t.Helper()
	dir, err := h.layout.CurrentDir(h.source)
	if err != nil {
		h.t.Fatalf("current dir: %v", err)
	}
	return dir
}

func (h *harness) currentFiles() map[string]string {
	h.t.Helper()
	files, err := relativeFiles(h.currentDir())
	if err != nil {
		h.t.Fatalf("read current tree: %v", err)
	}
	return files
}

func (h *harness) revisionFiles(runID string) map[string]string {
	h.t.Helper()
	dir, err := h.layout.RevisionDir(h.source.ID, runID)
	if err != nil {
		h.t.Fatalf("revision dir: %v", err)
	}
	files, err := relativeFiles(dir)
	if err != nil {
		h.t.Fatalf("read revision tree: %v", err)
	}
	return files
}

func (h *harness) collect() (domain.Run, domain.Source) {
	h.t.Helper()
	runID, err := h.service.StartCollection(context.Background(), h.source.ID)
	if err != nil {
		h.t.Fatalf("start collection: %v", err)
	}
	return h.reload(runID)
}

func (h *harness) reload(runID string) (domain.Run, domain.Source) {
	h.t.Helper()
	run, err := h.store.Run(context.Background(), runID)
	if err != nil || run == nil {
		h.t.Fatalf("load run: %v", err)
	}
	source, err := h.store.Source(context.Background(), h.source.ID)
	if err != nil || source == nil {
		h.t.Fatalf("load source: %v", err)
	}
	h.source = *source
	return *run, *source
}

func assertFiles(t *testing.T, label string, got map[string]string, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s holds %v, want %v", label, got, want)
	}
	for name, content := range want {
		if got[name] != content {
			t.Fatalf("%s[%s] = %q, want %q (full tree %v)", label, name, got[name], content, got)
		}
	}
}

// Scenario A: an initial collection into an empty current tree.
func TestCollectionInitialPopulatesCurrentAndCreatesNoRevisions(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.writeSource("b.txt", "B")

	run, source := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "A", "b.txt": "B"})
	if files := h.revisionFiles(run.ID); len(files) != 0 {
		t.Fatalf("revision batch should be empty, got %v", files)
	}
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status = %s, want STABLE", source.StagingStatus)
	}
	if source.StagingStateVersion != 1 {
		t.Fatalf("staging state version = %d, want 1", source.StagingStateVersion)
	}
	batch, err := h.store.RevisionBatchForRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read revision batch: %v", err)
	}
	if batch != nil {
		t.Fatalf("no revision batch should be recorded, got %#v", batch)
	}
}

// Scenario B: a changed source file. The previous version must be preserved.
func TestCollectionArchivesReplacedFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "OLD")
	first, _ := h.collect()
	if !first.Status.Successful() {
		t.Fatalf("seed run failed: %s", first.SafeError)
	}

	h.writeSource("a.txt", "NEW")
	run, source := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "NEW"})
	assertFiles(t, "revisions", h.revisionFiles(run.ID), map[string]string{"a.txt": "OLD"})
	if source.StagingStateVersion != 2 {
		t.Fatalf("staging state version = %d, want 2", source.StagingStateVersion)
	}

	batch, err := h.store.RevisionBatchForRun(context.Background(), run.ID)
	if err != nil || batch == nil {
		t.Fatalf("revision batch was not recorded: %v", err)
	}
	if !batch.ReconciliationCompleted {
		t.Fatal("a completed reconciliation must mark its revision batch complete")
	}
	if batch.ItemCount != 1 {
		t.Fatalf("batch item count = %d, want 1", batch.ItemCount)
	}
}

// Scenario C: a deleted source file. The staged copy must be archived, not lost.
func TestCollectionArchivesDeletedFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.writeSource("b.txt", "B")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.removeSource("b.txt")
	run, _ := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "A"})
	assertFiles(t, "revisions", h.revisionFiles(run.ID), map[string]string{"b.txt": "B"})
}

// A run that both replaces and removes a file must report every object it
// displaced, not just the ones that vanished from the source.
//
// Under an archiving policy nothing is deleted: each displaced object is moved
// into the revision batch, and the engine counts those moves as renames while
// counting a delete only for the object the source no longer has. Reporting the
// delete counter alone made the run claim one displaced object while its own
// revision batch durably recorded two, so the history contradicted the data.
func TestArchivingRunReportsEveryDisplacedObject(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("keep.txt", "KEEP")
	h.writeSource("change.txt", "OLD")
	h.writeSource("remove.txt", "GONE")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.writeSource("change.txt", "NEW")
	h.removeSource("remove.txt")
	run, _ := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"keep.txt": "KEEP", "change.txt": "NEW"})
	assertFiles(t, "revisions", h.revisionFiles(run.ID), map[string]string{"change.txt": "OLD", "remove.txt": "GONE"})

	batch, err := h.store.RevisionBatchForRun(context.Background(), run.ID)
	if err != nil || batch == nil {
		t.Fatalf("revision batch was not recorded: %v", err)
	}
	if batch.ItemCount != 2 {
		t.Fatalf("batch item count = %d, want 2", batch.ItemCount)
	}

	result, err := h.store.SourceResult(context.Background(), run.ID, h.source.ID)
	if err != nil || result == nil {
		t.Fatalf("source result was not recorded: %v", err)
	}
	if result.FilesDeletedOrArchived != batch.ItemCount {
		t.Fatalf("run reported %d displaced object(s) but its revision batch holds %d",
			result.FilesDeletedOrArchived, batch.ItemCount)
	}
}

// copy_only must never remove anything from the current tree.
func TestCopyOnlyNeverRemovesStagedData(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyCopyOnly)
	h.writeSource("a.txt", "A")
	h.writeSource("b.txt", "B")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.removeSource("b.txt")
	run, _ := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "A", "b.txt": "B"})
	batch, err := h.store.RevisionBatchForRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read revision batch: %v", err)
	}
	if batch != nil {
		t.Fatal("copy_only must not create a revision batch")
	}
}

// Scenario D: the source is unavailable. The last good staged copy must survive.
func TestUnavailableSourceLeavesCurrentIntact(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.writeSource("b.txt", "B")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.engine.connectErr = domain.NewError(domain.ErrClassUnavailable, "fake_unreachable",
		"the SMB server did not answer")
	run, source := h.collect()

	if run.Status.Successful() {
		t.Fatal("an unreachable source must not produce a successful run")
	}
	if run.ErrorClass != domain.ErrClassUnavailable {
		t.Fatalf("error class = %s, want UNAVAILABLE", run.ErrorClass)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "A", "b.txt": "B"})
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status = %s, want the previous STABLE state to be kept", source.StagingStatus)
	}
	if source.StagingStateVersion != 1 {
		t.Fatalf("staging state version = %d, want it unchanged at 1", source.StagingStateVersion)
	}
	if h.engine.collectCalls != 1 {
		t.Fatalf("the transfer ran %d times; it must not run at all when preflight fails", h.engine.collectCalls-1)
	}
}

// Scenario E: the source reports no files at all while staging holds data.
func TestUnexpectedlyEmptySourceIsBlockedBeforeAnyMutation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.writeSource("b.txt", "B")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}
	callsAfterSeed := h.engine.collectCalls

	h.removeSource("a.txt")
	h.removeSource("b.txt")
	run, source := h.collect()

	if run.Status.Successful() {
		t.Fatal("an unexpectedly empty source must not produce a successful run")
	}
	if run.ErrorCode != "source_unexpectedly_empty" {
		t.Fatalf("error code = %q, want source_unexpectedly_empty", run.ErrorCode)
	}
	assertFiles(t, "current", h.currentFiles(), map[string]string{"a.txt": "A", "b.txt": "B"})
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status = %s, want STABLE", source.StagingStatus)
	}
	if h.engine.collectCalls != callsAfterSeed {
		t.Fatal("the transfer must not run at all when the empty-source guard trips")
	}
}

// An empty source is allowed when staging is also empty: there is nothing to
// lose, so the guard must not block a legitimately empty share.
func TestEmptySourceIsAllowedWhenStagingIsAlsoEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	run, source := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status = %s, want STABLE", source.StagingStatus)
	}
}

// Scenario F: the transfer fails after it may have written into current. The
// run must never be reported successful and the source must not claim STABLE.
func TestInterruptedReconciliationNeverPublishesStable(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.writeSource("a.txt", "NEW")
	h.writeSource("c.txt", "C")
	h.engine.mutateBeforeFailing = true
	h.engine.collectErr = domain.NewError(domain.ErrClassExternalTool, "fake_engine_failed",
		"the transfer engine stopped before it finished")

	run, source := h.collect()

	if run.Status.Successful() {
		t.Fatalf("an interrupted reconciliation must not be reported as success, got %s", run.Status)
	}
	if source.StagingStatus != domain.StagingRecoveryRequired {
		t.Fatalf("staging status = %s, want RECOVERY_REQUIRED", source.StagingStatus)
	}
	if source.StagingStateVersion != 1 {
		t.Fatalf("staging state version = %d, want it not advanced past 1", source.StagingStateVersion)
	}

	result, err := h.store.SourceResult(context.Background(), run.ID, source.ID)
	if err != nil || result == nil {
		t.Fatalf("load source result: %v", err)
	}
	if result.StagingStateVersion != 0 {
		t.Fatalf("a failed run must not record a published state version, got %d", result.StagingStateVersion)
	}

	// A later clean run is the healing path and must restore a proven state.
	h.engine.collectErr = nil
	h.engine.mutateBeforeFailing = false
	healed, healedSource := h.collect()
	if !healed.Status.Successful() {
		t.Fatalf("healing run status = %s (%s)", healed.Status, healed.SafeError)
	}
	if healedSource.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status after healing = %s, want STABLE", healedSource.StagingStatus)
	}
	if healedSource.StagingStateVersion != 2 {
		t.Fatalf("staging state version after healing = %d, want 2", healedSource.StagingStateVersion)
	}
}

// A failed run whose engine produced no statistics cannot be proven harmless,
// so it must also land in RECOVERY_REQUIRED.
func TestFailureWithoutEngineStatisticsRequiresRecovery(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.engine.statsUnavailable = true
	h.engine.collectErr = domain.NewError(domain.ErrClassExternalTool, "fake_engine_failed",
		"the transfer engine failed")
	_, source := h.collect()

	if source.StagingStatus != domain.StagingRecoveryRequired {
		t.Fatalf("staging status = %s, want RECOVERY_REQUIRED when statistics are unavailable", source.StagingStatus)
	}
}

// An interrupted archiving run still records its batch, but marked incomplete
// so nothing can mistake it for a finished one.
func TestInterruptedRunRecordsIncompleteRevisionBatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "OLD")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	h.writeSource("a.txt", "NEW")
	h.engine.mutateBeforeFailing = true
	h.engine.collectErr = domain.NewError(domain.ErrClassExternalTool, "fake_engine_failed",
		"the transfer engine stopped before it finished")
	run, _ := h.collect()

	batch, err := h.store.RevisionBatchForRun(context.Background(), run.ID)
	if err != nil || batch == nil {
		t.Fatalf("an interrupted archiving run must still record what it displaced: %v", err)
	}
	if batch.ReconciliationCompleted {
		t.Fatal("an interrupted run must not mark its revision batch as a completed batch")
	}
	assertFiles(t, "revisions", h.revisionFiles(run.ID), map[string]string{"a.txt": "OLD"})
}

// Scenario G: the engine exits cleanly but its own counters contradict that.
// Each such signal must block publication outright, not merely add a warning:
// a tree the engine will not vouch for must never become STABLE, because that
// is the state a later backup consumes.
func TestEngineReportedFailureSignalsBlockPublication(t *testing.T) {
	t.Parallel()

	signals := map[string]func(*fakeEngine){
		"per-object errors": func(e *fakeEngine) { e.engineErrors = 2 },
		"fatal error flag":  func(e *fakeEngine) { e.fatalError = true },
		"retries exhausted": func(e *fakeEngine) { e.retryError = true },
		"incomplete queue":  func(e *fakeEngine) { e.extraQueuedTransfers = 3 },
	}
	for name, arrange := range signals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, domain.PolicyArchiveThenSync)
			h.writeSource("a.txt", "A")
			if run, _ := h.collect(); !run.Status.Successful() {
				t.Fatalf("seed run failed: %s", run.SafeError)
			}

			h.writeSource("a.txt", "NEW")
			arrange(h.engine)
			run, source := h.collect()

			if run.Status.Successful() {
				t.Fatalf("a run the engine will not vouch for must not succeed, got %s", run.Status)
			}
			if run.Status != domain.RunStatusFailed {
				t.Fatalf("run status = %s, want FAILED", run.Status)
			}
			if run.ErrorCode != "engine_reported_incomplete" {
				t.Fatalf("error code = %q, want engine_reported_incomplete", run.ErrorCode)
			}
			if source.StagingStatus == domain.StagingStable {
				t.Fatal("staging must not be advertised as STABLE after an incomplete transfer")
			}
			if source.StagingStateVersion != 1 {
				t.Fatalf("staging state version = %d, want it not advanced past 1", source.StagingStateVersion)
			}

			result, err := h.store.SourceResult(context.Background(), run.ID, source.ID)
			if err != nil || result == nil {
				t.Fatalf("load source result: %v", err)
			}
			if result.StagingStateVersion != 0 {
				t.Fatalf("an incomplete run must not record a published state version, got %d",
					result.StagingStateVersion)
			}
		})
	}
}

// The metrics an incomplete run did produce are still recorded, so history
// explains what happened rather than only that it failed.
func TestBlockedRunStillRecordsEngineMetrics(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.engine.engineErrors = 2

	run, _ := h.collect()
	result, err := h.store.SourceResult(context.Background(), run.ID, h.source.ID)
	if err != nil || result == nil {
		t.Fatalf("load source result: %v", err)
	}
	if result.EngineErrors != 2 {
		t.Fatalf("engine errors = %d, want 2", result.EngineErrors)
	}
	if result.FilesCopied == 0 {
		t.Fatal("the transfers that did happen must still be recorded")
	}
}

// A clean run with only advisory engine warnings still publishes, downgraded.
func TestAdvisoryWarningsStillPublish(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	h.engine.warnings = []string{"a file name was adjusted for the destination filesystem"}

	run, source := h.collect()
	if run.Status != domain.RunStatusSucceededWithWarning {
		t.Fatalf("run status = %s, want SUCCEEDED_WITH_WARNING", run.Status)
	}
	if source.StagingStatus != domain.StagingStable {
		t.Fatalf("staging status = %s, want STABLE", source.StagingStatus)
	}
}

// mirror is defined by the architecture but not implemented. It must be refused
// explicitly rather than silently behaving like another policy.
func TestMirrorPolicyIsRejectedExplicitly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	err := h.admin.Update(context.Background(), h.source.ID, SourceInput{
		Name:                 "documents",
		Host:                 "fileserver.example",
		ShareName:            "docs",
		StagingRelativePath:  "documents",
		Enabled:              true,
		ReconciliationPolicy: domain.PolicyMirror,
	})
	if err == nil {
		t.Fatal("selecting the mirror policy must be rejected")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "policy_unsupported" {
		t.Fatalf("error = %v, want an explicit policy_unsupported rejection", err)
	}

	// A source that somehow already carries the policy must still be refused at
	// execution time rather than falling through to another behaviour.
	h.source.ReconciliationPolicy = domain.PolicyMirror
	outcome := h.service.collect(context.Background(), domain.Run{ID: "run", SourceID: h.source.ID},
		h.source, h.source.CreatedAt)
	if outcome.Err == nil || outcome.Published {
		t.Fatal("an unsupported policy must never reach publication")
	}
}

// Two operations on the same source must not overlap.
func TestSourceIsExclusiveWhileBusy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	release, ok := h.service.locks.tryAcquire(h.source.ID)
	if !ok {
		t.Fatal("the source lock should be free")
	}
	defer release()

	if _, err := h.service.StartCollection(context.Background(), h.source.ID); err == nil {
		t.Fatal("a second operation on a busy source must be refused")
	}
	if !h.service.Busy(h.source.ID) {
		t.Fatal("a locked source must report as busy")
	}
}

// A disabled source must not be collectable.
func TestDisabledSourceIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	if err := h.admin.Update(context.Background(), h.source.ID, SourceInput{
		Name:                 "documents",
		Host:                 "fileserver.example",
		ShareName:            "docs",
		StagingRelativePath:  "documents",
		Enabled:              false,
		ReconciliationPolicy: domain.PolicyArchiveThenSync,
	}); err != nil {
		t.Fatalf("disable source: %v", err)
	}
	if _, err := h.service.StartCollection(context.Background(), h.source.ID); err == nil {
		t.Fatal("a disabled source must not be collectable")
	}
}

// A connection test is read-only: it must never create or change staging state.
func TestConnectionTestDoesNotTouchStaging(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")

	runID, err := h.service.StartConnectionTest(context.Background(), h.source.ID)
	if err != nil {
		t.Fatalf("start connection test: %v", err)
	}
	run, source := h.reload(runID)

	if run.Status != domain.RunStatusSucceeded {
		t.Fatalf("run status = %s (%s)", run.Status, run.SafeError)
	}
	if source.StagingStatus != domain.StagingEmpty {
		t.Fatalf("staging status = %s, want EMPTY", source.StagingStatus)
	}
	if source.StagingStateVersion != 0 {
		t.Fatalf("staging state version = %d, want 0", source.StagingStateVersion)
	}
	if files := h.currentFiles(); len(files) != 0 {
		t.Fatalf("a connection test must not write into staging, found %v", files)
	}
	if h.engine.collectCalls != 0 {
		t.Fatal("a connection test must not invoke a transfer")
	}
}

// A failed connection test is recorded with its error class so the UI can
// distinguish authentication from availability.
func TestConnectionTestRecordsTypedFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.engine.connectErr = domain.NewError(domain.ErrClassAuthentication, "fake_auth_failed",
		"the server rejected the credentials")

	runID, err := h.service.StartConnectionTest(context.Background(), h.source.ID)
	if err != nil {
		t.Fatalf("start connection test: %v", err)
	}
	run, _ := h.reload(runID)

	if run.Status != domain.RunStatusFailed {
		t.Fatalf("run status = %s, want FAILED", run.Status)
	}
	if run.ErrorClass != domain.ErrClassAuthentication {
		t.Fatalf("error class = %s, want AUTHENTICATION", run.ErrorClass)
	}
}

// Startup recovery must never leave a source claiming a state it cannot prove.
func TestRecoveryMarksStaleUpdatingSourceAsRecoveryRequired(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	if err := h.store.SetSourceStagingStatus(context.Background(), h.source.ID, domain.StagingUpdating); err != nil {
		t.Fatalf("simulate interrupted reconciliation: %v", err)
	}

	report, err := h.service.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.RecoveredSources != 1 {
		t.Fatalf("recovered sources = %d, want 1", report.RecoveredSources)
	}
	source, err := h.store.Source(context.Background(), h.source.ID)
	if err != nil || source == nil {
		t.Fatalf("load source: %v", err)
	}
	if source.StagingStatus != domain.StagingRecoveryRequired {
		t.Fatalf("staging status = %s, want RECOVERY_REQUIRED", source.StagingStatus)
	}
}

// A source that looks stable but has a work area recording an interrupted
// mutating run is not actually provable and must be flagged too.
func TestRecoveryDetectsMutatingWorkAreaOnAStableSource(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	runID, err := domain.NewID()
	if err != nil {
		t.Fatalf("generate run id: %v", err)
	}
	workDir, err := h.layout.WorkDir(h.source.ID, runID)
	if err != nil {
		t.Fatalf("work dir: %v", err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("create work dir: %v", err)
	}
	if err := staging.WriteMarker(workDir, staging.RunMarker{
		RunID:    runID,
		SourceID: h.source.ID,
		Phase:    staging.PhaseMutating,
	}); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if _, err := h.service.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	source, err := h.store.Source(context.Background(), h.source.ID)
	if err != nil || source == nil {
		t.Fatalf("load source: %v", err)
	}
	if source.StagingStatus != domain.StagingRecoveryRequired {
		t.Fatalf("staging status = %s, want RECOVERY_REQUIRED", source.StagingStatus)
	}
}

// Runs left active by a previous process must become INTERRUPTED.
func TestRecoveryInterruptsActiveRuns(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	runID, err := domain.NewID()
	if err != nil {
		t.Fatalf("generate run id: %v", err)
	}
	if err := h.store.RecordRun(context.Background(), domain.Run{
		ID:          runID,
		Kind:        domain.RunKindCollection,
		TriggerKind: domain.RunTriggerManual,
		SourceID:    h.source.ID,
		Status:      domain.RunStatusRunning,
		RequestedAt: h.source.CreatedAt,
		UpdatedAt:   h.source.CreatedAt,
	}); err != nil {
		t.Fatalf("record active run: %v", err)
	}

	report, err := h.service.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.InterruptedRuns != 1 {
		t.Fatalf("interrupted runs = %d, want 1", report.InterruptedRuns)
	}
	run, err := h.store.Run(context.Background(), runID)
	if err != nil || run == nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != domain.RunStatusInterrupted {
		t.Fatalf("run status = %s, want INTERRUPTED", run.Status)
	}
}

// A source that has lost most, but not all, of its files is allowed through
// because archiving preserves the displaced data, but the run must say so.
func TestLargeSourceShrinkageIsWarnedAboutButNotBlocked(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	for i := 0; i < 20; i++ {
		h.writeSource(fmt.Sprintf("file-%02d.txt", i), "x")
	}
	if run, _ := h.collect(); !run.Status.Successful() {
		t.Fatalf("seed run failed: %s", run.SafeError)
	}

	for i := 2; i < 20; i++ {
		h.removeSource(fmt.Sprintf("file-%02d.txt", i))
	}
	run, source := h.collect()

	if !run.Status.Successful() {
		t.Fatalf("a shrinking source must not be blocked outright: %s", run.SafeError)
	}
	if run.Status != domain.RunStatusSucceededWithWarning {
		t.Fatalf("run status = %s, want SUCCEEDED_WITH_WARNING", run.Status)
	}
	result, err := h.store.SourceResult(context.Background(), run.ID, source.ID)
	if err != nil || result == nil {
		t.Fatalf("load source result: %v", err)
	}
	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "archived into this run's revision batch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the shrinkage warning is missing: %#v", result.Warnings)
	}
	// The displaced files are preserved, which is why this is a warning.
	if files := h.revisionFiles(run.ID); len(files) != 18 {
		t.Fatalf("revision batch holds %d files, want 18", len(files))
	}
}
