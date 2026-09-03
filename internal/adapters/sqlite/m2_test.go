package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func migratedStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func seedSource(t *testing.T, store *Store, name, stagingPath string) domain.Source {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	source := domain.Source{
		ID:                   id,
		Name:                 name,
		Host:                 "fileserver.example",
		ShareName:            "docs",
		Username:             "reader",
		StagingRelativePath:  stagingPath,
		Enabled:              true,
		ReconciliationPolicy: domain.PolicyArchiveThenSync,
	}
	if err := store.CreateSource(context.Background(), source); err != nil {
		t.Fatalf("create source: %v", err)
	}
	loaded, err := store.Source(context.Background(), id)
	if err != nil || loaded == nil {
		t.Fatalf("load source: %v", err)
	}
	return *loaded
}

// The M1 database must upgrade in place, and its DEMO history must survive.
func TestExistingM1DatabaseUpgradesAndKeepsItsDemoRow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rotup.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	demoTime := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

	// Build a database that only has migration 001 applied, exactly as an M1
	// deployment would have left it.
	first, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	migrations, err := readMigrations()
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL,
			checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	if err := first.applyMigration(ctx, migrations[0]); err != nil {
		t.Fatalf("apply migration 001: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, status, summary, requested_at, started_at, finished_at)
		VALUES (?, 'DEMO', 'SUCCEEDED', ?, ?, ?, ?)`,
		"00000000-0000-4000-8000-000000000001",
		"M1 demo run: this is not a backup",
		formatTime(demoTime), formatTime(demoTime), formatTime(demoTime),
	); err != nil {
		t.Fatalf("insert M1 demo row: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening and migrating must move it forward without losing anything.
	store, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade an M1 database: %v", err)
	}
	if applied != len(migrations)-1 {
		t.Fatalf("applied %d migrations, want %d", applied, len(migrations)-1)
	}

	run, err := store.Run(ctx, "00000000-0000-4000-8000-000000000001")
	if err != nil || run == nil {
		t.Fatalf("the DEMO run did not survive the upgrade: %v", err)
	}
	if run.Kind != domain.RunKindDemo || run.Status != domain.RunStatusSucceeded {
		t.Fatalf("migrated run = %#v", run)
	}
	if !strings.Contains(run.Summary, "not a backup") {
		t.Fatalf("the DEMO summary was lost: %q", run.Summary)
	}
	if !run.RequestedAt.Equal(demoTime) || !run.FinishedAt.Equal(demoTime) {
		t.Fatalf("the DEMO timestamps changed: %#v", run)
	}
	if run.TriggerKind != domain.RunTriggerManual {
		t.Fatalf("trigger kind = %q, want the M2 default", run.TriggerKind)
	}

	// The upgraded schema must accept the M2 run kinds.
	source := seedSource(t, store, "documents", "documents")
	for _, kind := range []domain.RunKind{domain.RunKindConnectionTest, domain.RunKindCollection} {
		id, err := domain.NewID()
		if err != nil {
			t.Fatalf("new id: %v", err)
		}
		if err := store.RecordRun(ctx, domain.Run{
			ID: id, Kind: kind, SourceID: source.ID, Status: domain.RunStatusPending,
			RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record %s run: %v", kind, err)
		}
	}

	assertConsistent(t, store)
}

func assertConsistent(t *testing.T, store *Store) {
	t.Helper()
	violations, err := store.ForeignKeyCheck(context.Background())
	if err != nil {
		t.Fatalf("foreign key check: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("foreign key violations: %v", violations)
	}
	if err := store.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("integrity check: %v", err)
	}
}

func TestSourceCRUD(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "team/documents")

	if source.StagingStatus != domain.StagingEmpty || source.StagingStateVersion != 0 {
		t.Fatalf("a new source starts at %s/%d, want EMPTY/0", source.StagingStatus, source.StagingStateVersion)
	}
	if source.HasCredential() {
		t.Fatal("a new source has no credential")
	}

	source.Name = "team documents"
	source.Subdirectory = "reports"
	source.ExcludeRules = []string{"*.tmp"}
	source.IncludeRules = []string{"*.pdf"}
	source.Enabled = false
	source.ReconciliationPolicy = domain.PolicyCopyOnly
	if err := store.UpdateSourceSettings(ctx, source); err != nil {
		t.Fatalf("update source: %v", err)
	}

	loaded, err := store.Source(ctx, source.ID)
	if err != nil || loaded == nil {
		t.Fatalf("reload source: %v", err)
	}
	if loaded.Name != "team documents" || loaded.Subdirectory != "reports" || loaded.Enabled {
		t.Fatalf("updated source = %#v", loaded)
	}
	if len(loaded.ExcludeRules) != 1 || loaded.ExcludeRules[0] != "*.tmp" {
		t.Fatalf("exclude rules = %#v", loaded.ExcludeRules)
	}
	if loaded.ReconciliationPolicy != domain.PolicyCopyOnly {
		t.Fatalf("policy = %s", loaded.ReconciliationPolicy)
	}

	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("listed %d sources, want 1", len(sources))
	}

	paths, err := store.StagingPaths(ctx)
	if err != nil {
		t.Fatalf("staging paths: %v", err)
	}
	if paths["team/documents"] != source.ID {
		t.Fatalf("staging destinations = %#v", paths)
	}

	missing, err := store.Source(ctx, "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatalf("load missing source: %v", err)
	}
	if missing != nil {
		t.Fatal("an unknown source must read back as nil, not an error")
	}
	assertConsistent(t, store)
}

func TestDuplicateSourceNamesAndDestinationsAreRejected(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	first := seedSource(t, store, "documents", "documents")

	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	duplicateName := first
	duplicateName.ID = id
	duplicateName.StagingRelativePath = "other"
	err = store.CreateSource(context.Background(), duplicateName)
	if err == nil {
		t.Fatal("a duplicate source name must be rejected")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "source_name_taken" {
		t.Fatalf("error = %v, want a source_name_taken rejection", err)
	}

	duplicatePath := first
	duplicatePath.ID = id
	duplicatePath.Name = "other"
	err = store.CreateSource(context.Background(), duplicatePath)
	if err == nil {
		t.Fatal("a duplicate staging destination must be rejected")
	}
	if !domain.AsError(err, &typed) || typed.Code != "staging_path_taken" {
		t.Fatalf("error = %v, want a staging_path_taken rejection", err)
	}
}

func TestCredentialReferenceLifecycle(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	if ref, err := store.CredentialRef(ctx, source.ID); err != nil || ref.ID != "" {
		t.Fatalf("a source without a credential must return an empty reference: %#v %v", ref, err)
	}

	firstID, _ := domain.NewID()
	displaced, err := store.AttachCredential(ctx, source.ID, domain.CredentialRef{
		ID: firstID, DisplayName: "SMB password", Provider: "file",
		ProviderLocator: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Purpose: domain.SecretPurposeSMBPassword,
	})
	if err != nil {
		t.Fatalf("attach credential: %v", err)
	}
	if displaced != "" {
		t.Fatalf("nothing should have been displaced, got %q", displaced)
	}

	secondID, _ := domain.NewID()
	displaced, err = store.AttachCredential(ctx, source.ID, domain.CredentialRef{
		ID: secondID, DisplayName: "SMB password", Provider: "file",
		ProviderLocator: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Purpose: domain.SecretPurposeSMBPassword,
	})
	if err != nil {
		t.Fatalf("replace credential: %v", err)
	}
	if displaced != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("displaced locator = %q, want the first one", displaced)
	}

	ref, err := store.CredentialRef(ctx, source.ID)
	if err != nil {
		t.Fatalf("read credential reference: %v", err)
	}
	if ref.ID != secondID || ref.ProviderLocator != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("credential reference = %#v", ref)
	}

	cleared, err := store.ClearCredential(ctx, source.ID)
	if err != nil {
		t.Fatalf("clear credential: %v", err)
	}
	if cleared != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("cleared locator = %q", cleared)
	}
	loaded, err := store.Source(ctx, source.ID)
	if err != nil || loaded == nil {
		t.Fatalf("reload source: %v", err)
	}
	if loaded.HasCredential() {
		t.Fatal("the source must no longer reference a credential")
	}
	assertConsistent(t, store)
}

func TestRunAndSourceResultPersistence(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	runID, _ := domain.NewID()
	requested := time.Now().UTC().Round(time.Millisecond)
	if err := store.RecordRun(ctx, domain.Run{
		ID: runID, Kind: domain.RunKindCollection, TriggerKind: domain.RunTriggerManual,
		SourceID: source.ID, Status: domain.RunStatusPending,
		Summary: "requested", RequestedAt: requested, UpdatedAt: requested,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	// A pending run has no start or finish time yet.
	pending, err := store.Run(ctx, runID)
	if err != nil || pending == nil {
		t.Fatalf("load run: %v", err)
	}
	if !pending.StartedAt.IsZero() || !pending.FinishedAt.IsZero() {
		t.Fatalf("a pending run must not carry timestamps: %#v", pending)
	}
	if pending.Status.Terminal() {
		t.Fatal("PENDING must not be a terminal status")
	}

	active, err := store.ActiveRuns(ctx)
	if err != nil {
		t.Fatalf("active runs: %v", err)
	}
	if len(active) != 1 || active[0].ID != runID {
		t.Fatalf("active runs = %#v", active)
	}

	finished := requested.Add(3 * time.Second)
	pending.Status = domain.RunStatusSucceededWithWarning
	pending.StartedAt = requested
	pending.FinishedAt = finished
	pending.Summary = "collected 2 files"
	pending.SummaryJSON = `{"files_copied":2}`
	if err := store.UpdateRun(ctx, *pending); err != nil {
		t.Fatalf("update run: %v", err)
	}

	reloaded, err := store.Run(ctx, runID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.Status != domain.RunStatusSucceededWithWarning || !reloaded.Status.Successful() {
		t.Fatalf("run status = %s", reloaded.Status)
	}
	if reloaded.Duration() != 3*time.Second {
		t.Fatalf("duration = %v, want 3s", reloaded.Duration())
	}
	if active, _ := store.ActiveRuns(ctx); len(active) != 0 {
		t.Fatalf("a terminal run must not stay active: %#v", active)
	}

	published := finished
	updated := source
	updated.StagingStatus = domain.StagingStable
	updated.StagingStateVersion = 1
	updated.LastPublishedAt = &published
	updated.LastCollectionRunID = runID
	result := domain.SourceResult{
		RunID: runID, SourceID: source.ID, Status: domain.RunStatusSucceededWithWarning,
		StagingStateVersion: 1, PublishedAt: published, StartedAt: requested, FinishedAt: finished,
		BytesScanned: 100, BytesCopied: 80, FilesScanned: 4, FilesCopied: 2,
		FilesDeletedOrArchived: 1, EngineChecks: 2, EngineErrors: 0,
		Warnings: []string{"one file was skipped"},
	}
	if err := store.SaveSourceResult(ctx, result, &updated); err != nil {
		t.Fatalf("save source result: %v", err)
	}

	loadedResult, err := store.SourceResult(ctx, runID, source.ID)
	if err != nil || loadedResult == nil {
		t.Fatalf("load source result: %v", err)
	}
	if loadedResult.FilesCopied != 2 || loadedResult.BytesCopied != 80 || loadedResult.StagingStateVersion != 1 {
		t.Fatalf("result = %#v", loadedResult)
	}
	if len(loadedResult.Warnings) != 1 || loadedResult.Warnings[0] != "one file was skipped" {
		t.Fatalf("warnings = %#v", loadedResult.Warnings)
	}

	loadedSource, err := store.Source(ctx, source.ID)
	if err != nil || loadedSource == nil {
		t.Fatalf("reload source: %v", err)
	}
	if loadedSource.StagingStatus != domain.StagingStable || loadedSource.StagingStateVersion != 1 {
		t.Fatalf("the source state was not advanced with the result: %#v", loadedSource)
	}
	if loadedSource.LastPublishedAt == nil {
		t.Fatal("the publication time was not recorded")
	}

	latest, err := store.LatestCollectionForSource(ctx, source.ID)
	if err != nil || latest == nil || latest.ID != runID {
		t.Fatalf("latest collection = %#v (%v)", latest, err)
	}
	runs, err := store.ListRunsForSource(ctx, source.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs for source = %#v (%v)", runs, err)
	}
	assertConsistent(t, store)
}

func TestSourceStagingStatusTransitions(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	for _, status := range []domain.StagingStatus{
		domain.StagingUpdating, domain.StagingStable,
		domain.StagingRecoveryRequired, domain.StagingEmpty,
	} {
		if err := store.SetSourceStagingStatus(ctx, source.ID, status); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
		loaded, err := store.Source(ctx, source.ID)
		if err != nil || loaded == nil {
			t.Fatalf("reload source: %v", err)
		}
		if loaded.StagingStatus != status {
			t.Fatalf("status = %s, want %s", loaded.StagingStatus, status)
		}
		if loaded.StagingStateVersion != 0 {
			t.Fatal("a status transition must not advance the state version")
		}
	}

	// The schema itself must refuse a status ROTUP does not define.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE sources SET staging_status = 'PROBABLY_FINE' WHERE id = ?`, source.ID); err == nil {
		t.Fatal("the schema must reject an undefined staging status")
	}
}

func TestRevisionBatchPersistence(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	runID, _ := domain.NewID()
	if err := store.RecordRun(ctx, domain.Run{
		ID: runID, Kind: domain.RunKindCollection, SourceID: source.ID,
		Status: domain.RunStatusFailed, RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	batchID, _ := domain.NewID()
	batch := domain.RevisionBatch{
		ID: batchID, SourceID: source.ID, CollectionRunID: runID,
		RelativePath: "revisions/" + source.ID + "/" + runID,
		CreatedAt:    time.Now().UTC(), ItemCount: 3, ByteCount: 900,
		ReconciliationCompleted: false,
	}
	if err := store.RecordRevisionBatch(ctx, batch); err != nil {
		t.Fatalf("record revision batch: %v", err)
	}

	loaded, err := store.RevisionBatchForRun(ctx, runID)
	if err != nil || loaded == nil {
		t.Fatalf("load revision batch: %v", err)
	}
	if loaded.ItemCount != 3 || loaded.ByteCount != 900 {
		t.Fatalf("batch = %#v", loaded)
	}
	if loaded.ReconciliationCompleted {
		t.Fatal("an interrupted run's batch must not read back as completed")
	}

	// Re-recording the same batch updates it rather than duplicating it.
	batch.ItemCount = 4
	batch.ReconciliationCompleted = true
	if err := store.RecordRevisionBatch(ctx, batch); err != nil {
		t.Fatalf("re-record revision batch: %v", err)
	}
	batches, err := store.ListRevisionBatches(ctx, source.ID, 10)
	if err != nil {
		t.Fatalf("list revision batches: %v", err)
	}
	if len(batches) != 1 || batches[0].ItemCount != 4 || !batches[0].ReconciliationCompleted {
		t.Fatalf("batches = %#v", batches)
	}
	assertConsistent(t, store)
}

func TestEventsAreAppendOnlyAndBounded(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	if err := store.AppendEvent(ctx, domain.Event{
		OccurredAt: time.Now().UTC(), Severity: domain.SeverityWarning,
		EventType: "collection.blocked_empty_source", Component: "collector",
		SourceID: source.ID, PayloadJSON: `{"staged_files":2}`,
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	// An oversized payload is replaced rather than stored.
	if err := store.AppendEvent(ctx, domain.Event{
		OccurredAt: time.Now().UTC(), Severity: domain.SeverityInfo,
		EventType: "test.large", Component: "collector",
		PayloadJSON: `{"x":"` + strings.Repeat("y", maxEventPayload) + `"}`,
	}); err != nil {
		t.Fatalf("append large event: %v", err)
	}

	events, err := store.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("listed %d events, want 2", len(events))
	}
	for _, event := range events {
		if len(event.PayloadJSON) > maxEventPayload {
			t.Fatalf("an event payload of %d bytes exceeded the bound", len(event.PayloadJSON))
		}
	}

	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO events (occurred_at, severity, event_type, component)
		 VALUES ('2026-01-01T00:00:00Z', 'CHATTY', 'x', 'y')`); err == nil {
		t.Fatal("the schema must reject an undefined severity")
	}
	assertConsistent(t, store)
}

func TestOneStateVersionPerSource(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")

	record := func(status domain.RunStatus, version int64) error {
		runID, _ := domain.NewID()
		if err := store.RecordRun(ctx, domain.Run{
			ID: runID, Kind: domain.RunKindCollection, SourceID: source.ID,
			Status: status, RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return store.SaveSourceResult(ctx, domain.SourceResult{
			RunID: runID, SourceID: source.ID, Status: status, StagingStateVersion: version,
		}, nil)
	}

	if err := record(domain.RunStatusSucceeded, 1); err != nil {
		t.Fatalf("record first published result: %v", err)
	}
	if err := record(domain.RunStatusSucceeded, 1); err == nil {
		t.Fatal("two runs must not be able to claim the same staging state version")
	}
	// Unpublished results carry no version and may coexist freely.
	if err := record(domain.RunStatusFailed, 0); err != nil {
		t.Fatalf("record first failed result: %v", err)
	}
	if err := record(domain.RunStatusFailed, 0); err != nil {
		t.Fatalf("record second failed result: %v", err)
	}
	assertConsistent(t, store)
}

func TestMigrationChecksumDriftIsRefused(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	if _, err := store.db.Exec(`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper with the ledger: %v", err)
	}
	if _, err := store.Migrate(context.Background()); err == nil {
		t.Fatal("a changed migration checksum must refuse to start")
	}
}

func TestDatabaseNewerThanTheBinaryIsRefused(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	if _, err := store.db.Exec(`
		INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (999, '999_future.sql', 'x', '2030-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	if _, err := store.Migrate(context.Background()); err == nil {
		t.Fatal("a database newer than the binary must refuse to start")
	}
}
