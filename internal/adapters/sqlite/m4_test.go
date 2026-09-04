package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const fixtureResticID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const fixtureSnapshotID = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func seedRepository(t *testing.T, store *Store, disk domain.Disk) domain.Repository {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	now := time.Now().UTC()
	repository := domain.Repository{
		ID:             id,
		DiskID:         disk.ID,
		FilesystemUUID: disk.FilesystemUUID,
		RelativePath:   domain.RepositoryRelativePath,
		Origin:         domain.RepositoryInitialized,
		Health:         domain.RepositoryUninitialized,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateRepository(context.Background(), repository); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	return repository
}

func seedBackupRun(t *testing.T, store *Store, repository domain.Repository, sourceIDs []string) domain.Run {
	t.Helper()
	ctx := context.Background()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID:          id,
		Kind:        domain.RunKindBackupLatest,
		TriggerKind: domain.RunTriggerManual,
		Status:      domain.RunStatusRunning,
		RequestedAt: now,
		StartedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.RecordRun(ctx, run); err != nil {
		t.Fatalf("record backup run: %v", err)
	}
	if err := store.CreateBackupRun(ctx, domain.BackupRun{
		RunID:             run.ID,
		RepositoryID:      repository.ID,
		DiskID:            repository.DiskID,
		Stage:             domain.StageRequested,
		SelectedSourceIDs: sourceIDs,
	}); err != nil {
		t.Fatalf("create backup detail: %v", err)
	}
	return run
}

// An M3 deployment has to move to M4 in place, keeping every run, source, disk
// and revision batch it already had. The runs table is rebuilt by this
// migration, so this is the case most likely to lose data if it is wrong.
func TestExistingM3DatabaseUpgradesAndKeepsItsHistory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rotup.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	collectedAt := time.Date(2026, time.September, 2, 9, 30, 0, 0, time.UTC)

	migrations, err := readMigrations()
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if len(migrations) < 4 {
		t.Fatalf("expected at least four migrations, found %d", len(migrations))
	}

	// Build a database with only migrations 001 to 003 applied, exactly as an
	// M3 deployment would have left it, and put real rows in it.
	first, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL,
			checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	for _, migration := range migrations[:3] {
		if err := first.applyMigration(ctx, migration); err != nil {
			t.Fatalf("apply migration %03d: %v", migration.version, err)
		}
	}

	source := seedSource(t, first, "documents", "documents")
	disk := seedDisk(t, first, "disk-a", "11111111-2222-3333-4444-555555555555")
	collectionRunID := "00000000-0000-4000-8000-0000000000c1"
	if _, err := first.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, source_id, status, summary, summary_json,
			requested_at, started_at, finished_at, updated_at)
		VALUES (?, 'COLLECTION', 'MANUAL', ?, 'SUCCEEDED', 'collected 10 files', '{}', ?, ?, ?, ?)`,
		collectionRunID, source.ID,
		formatTime(collectedAt), formatTime(collectedAt), formatTime(collectedAt), formatTime(collectedAt),
	); err != nil {
		t.Fatalf("insert M3 collection run: %v", err)
	}
	batchID := "00000000-0000-4000-8000-0000000000b1"
	if _, err := first.db.ExecContext(ctx, `
		INSERT INTO revision_batches (id, source_id, collection_run_id, relative_path,
			created_at, item_count, byte_count, reconciliation_completed)
		VALUES (?, ?, ?, ?, ?, 4, 2048, 1)`,
		batchID, source.ID, collectionRunID,
		"revisions/"+source.ID+"/"+collectionRunID, formatTime(collectedAt),
	); err != nil {
		t.Fatalf("insert M3 revision batch: %v", err)
	}
	if _, err := first.db.ExecContext(ctx, `
		INSERT INTO events (occurred_at, severity, event_type, component, source_id, disk_id, run_id, payload_json)
		VALUES (?, 'INFO', 'media.detected', 'removable', NULL, ?, ?, '{}')`,
		formatTime(collectedAt), disk.ID, collectionRunID,
	); err != nil {
		t.Fatalf("insert M3 event: %v", err)
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
		t.Fatalf("upgrade an M3 database: %v", err)
	}
	if applied != len(migrations)-3 {
		t.Fatalf("applied %d migrations, want %d", applied, len(migrations)-3)
	}

	run, err := store.Run(ctx, collectionRunID)
	if err != nil || run == nil {
		t.Fatalf("the M3 collection run did not survive the upgrade: %v", err)
	}
	if run.Kind != domain.RunKindCollection || run.SourceID != source.ID {
		t.Fatalf("migrated run = %#v", run)
	}
	if !run.RequestedAt.Equal(collectedAt) || !run.FinishedAt.Equal(collectedAt) {
		t.Fatalf("the run timestamps changed: %#v", run)
	}
	if run.Summary != "collected 10 files" {
		t.Fatalf("the run summary was lost: %q", run.Summary)
	}

	batches, err := store.ListRevisionBatches(ctx, source.ID, 10)
	if err != nil || len(batches) != 1 || batches[0].ID != batchID {
		t.Fatalf("the revision batch did not survive the upgrade: %v %v", batches, err)
	}
	events, err := store.ListEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0].DiskID != disk.ID {
		t.Fatalf("the event did not survive the upgrade: %v %v", events, err)
	}
	disks, err := store.ListDisks(ctx)
	if err != nil || len(disks) != 1 {
		t.Fatalf("the registered disk did not survive the upgrade: %v %v", disks, err)
	}

	// The upgraded schema must accept the M4 run kinds.
	for _, kind := range []domain.RunKind{
		domain.RunKindDiskSetup, domain.RunKindBackupLatest, domain.RunKindVerify,
	} {
		id, err := domain.NewID()
		if err != nil {
			t.Fatalf("new id: %v", err)
		}
		if err := store.RecordRun(ctx, domain.Run{
			ID: id, Kind: kind, Status: domain.RunStatusPending,
			RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record %s run: %v", kind, err)
		}
	}

	assertConsistent(t, store)
}

func TestRepositoryLifecyclePersists(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)

	loaded, err := store.RepositoryForDisk(ctx, disk.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load repository by disk: %v", err)
	}
	if loaded.Health != domain.RepositoryUninitialized || loaded.Established() {
		t.Fatalf("a fresh repository should be unestablished: %#v", loaded)
	}

	established := time.Now().UTC()
	if err := store.SetRepositoryEstablished(ctx, repository.ID, fixtureResticID,
		domain.RepositoryHealthy, established); err != nil {
		t.Fatalf("set established: %v", err)
	}
	loaded, err = store.Repository(ctx, repository.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load repository: %v", err)
	}
	if loaded.ResticRepositoryID != fixtureResticID || !loaded.Established() {
		t.Fatalf("repository identity was not persisted: %#v", loaded)
	}
	if loaded.Health != domain.RepositoryHealthy {
		t.Fatalf("repository health = %s", loaded.Health)
	}

	if err := store.SetRepositoryHealth(ctx, repository.ID, domain.RepositorySuspect,
		"an interrupted backup"); err != nil {
		t.Fatalf("set health: %v", err)
	}
	loaded, _ = store.Repository(ctx, repository.ID)
	if loaded.Health != domain.RepositorySuspect || loaded.SuspectReason != "an interrupted backup" {
		t.Fatalf("suspect state was not persisted: %#v", loaded)
	}

	// Returning to healthy must clear the reason, so a stale explanation cannot
	// outlive the problem it described.
	if err := store.SetRepositoryHealth(ctx, repository.ID, domain.RepositoryHealthy, "ignored"); err != nil {
		t.Fatalf("clear health: %v", err)
	}
	loaded, _ = store.Repository(ctx, repository.ID)
	if loaded.SuspectReason != "" {
		t.Fatalf("the suspect reason survived a return to healthy: %q", loaded.SuspectReason)
	}

	assertConsistent(t, store)
}

// A disk holds one repository. A second would make "the repository on this
// disk" ambiguous, which every identity check depends on not being.
func TestOneRepositoryPerDisk(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	seedRepository(t, store, disk)

	id, _ := domain.NewID()
	err := store.CreateRepository(context.Background(), domain.Repository{
		ID: id, DiskID: disk.ID, FilesystemUUID: disk.FilesystemUUID,
		RelativePath: domain.RepositoryRelativePath,
		Origin:       domain.RepositoryInitialized, Health: domain.RepositoryUninitialized,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("a second repository was created on the same disk")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "repository_already_exists" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRepositoryCredentialRoundTrip(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)

	firstRefID, _ := domain.NewID()
	displaced, err := store.AttachRepositoryCredential(ctx, repository.ID, domain.CredentialRef{
		ID: firstRefID, DisplayName: "repository password", Provider: domain.SecretProviderFile,
		ProviderLocator: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Purpose:         domain.SecretPurposeRepositoryPassword,
	})
	if err != nil {
		t.Fatalf("attach credential: %v", err)
	}
	if displaced != "" {
		t.Fatalf("a first attachment displaced %q", displaced)
	}

	ref, err := store.RepositoryCredentialRef(ctx, repository.ID)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if ref.ProviderLocator != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("credential locator = %q", ref.ProviderLocator)
	}
	if ref.Purpose != domain.SecretPurposeRepositoryPassword {
		t.Fatalf("credential purpose = %q", ref.Purpose)
	}

	secondRefID, _ := domain.NewID()
	displaced, err = store.AttachRepositoryCredential(ctx, repository.ID, domain.CredentialRef{
		ID: secondRefID, DisplayName: "repository password", Provider: domain.SecretProviderFile,
		ProviderLocator: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Purpose:         domain.SecretPurposeRepositoryPassword,
	})
	if err != nil {
		t.Fatalf("replace credential: %v", err)
	}
	if displaced != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("displaced locator = %q; the old material would be orphaned", displaced)
	}
	assertConsistent(t, store)
}

// The read-set is the record of what a run actually read. It has to survive
// exactly as written, because coverage is derived from it.
func TestReadSetAndCoveragePersistExactly(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)
	source := seedSource(t, store, "documents", "documents")

	collectionRun := domain.Run{
		ID: mustID(t), Kind: domain.RunKindCollection, SourceID: source.ID,
		Status: domain.RunStatusSucceeded, RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.RecordRun(ctx, collectionRun); err != nil {
		t.Fatalf("record collection run: %v", err)
	}
	batch := domain.RevisionBatch{
		ID: mustID(t), SourceID: source.ID, CollectionRunID: collectionRun.ID,
		RelativePath: "revisions/" + source.ID + "/" + collectionRun.ID,
		CreatedAt:    time.Now().UTC(), ItemCount: 3, ByteCount: 512,
		ReconciliationCompleted: true,
	}
	if err := store.RecordRevisionBatch(ctx, batch); err != nil {
		t.Fatalf("record batch: %v", err)
	}

	run := seedBackupRun(t, store, repository, []string{source.ID})
	captured := time.Now().UTC().Truncate(time.Millisecond)
	states := []domain.BackupSourceState{{
		BackupRunID: run.ID, SourceID: source.ID,
		CurrentCollectionRunID: collectionRun.ID, StagingStateVersion: 7,
		PublishedAt: captured, CurrentRelativePath: "sources/documents/current", CapturedAt: captured,
	}}
	batches := []domain.BackupRevisionBatch{{
		BackupRunID: run.ID, RevisionBatchID: batch.ID, SourceID: source.ID,
		RelativePath: batch.RelativePath, BatchCreatedAt: batch.CreatedAt, CapturedAt: captured,
	}}
	if err := store.SaveReadSet(ctx, run.ID, states, batches); err != nil {
		t.Fatalf("save read-set: %v", err)
	}

	loadedStates, err := store.BackupSourceStates(ctx, run.ID)
	if err != nil || len(loadedStates) != 1 {
		t.Fatalf("load source states: %v %v", loadedStates, err)
	}
	if loadedStates[0].StagingStateVersion != 7 ||
		loadedStates[0].CurrentCollectionRunID != collectionRun.ID ||
		loadedStates[0].CurrentRelativePath != "sources/documents/current" {
		t.Fatalf("the read-set was not persisted exactly: %#v", loadedStates[0])
	}
	loadedBatches, err := store.BackupRevisionBatches(ctx, run.ID)
	if err != nil || len(loadedBatches) != 1 || loadedBatches[0].RevisionBatchID != batch.ID {
		t.Fatalf("the revision read-set was not persisted exactly: %v %v", loadedBatches, err)
	}

	// A backup that has not proven itself covers nothing.
	coverage, err := store.RevisionCoverageForBatch(ctx, batch.ID)
	if err != nil || len(coverage) != 0 {
		t.Fatalf("an unproven backup recorded coverage: %v %v", coverage, err)
	}

	now := time.Now().UTC()
	run.Status = domain.RunStatusSucceeded
	run.FinishedAt = now
	err = store.CompleteBackupRun(ctx, run, domain.BackupRun{
		RunID: run.ID, RepositoryID: repository.ID, DiskID: disk.ID,
		VerifiedSnapshotID: fixtureSnapshotID, Durability: domain.DurabilityNormal,
		DurableSuccessAt: now, SyncedAt: now, UnmountVerifiedAt: now,
	}, []domain.RevisionCoverage{{
		RevisionBatchID: batch.ID, BackupRunID: run.ID, RepositoryID: repository.ID,
		DiskID: disk.ID, SnapshotID: fixtureSnapshotID, CoveredAt: now,
	}})
	if err != nil {
		t.Fatalf("complete backup: %v", err)
	}

	coverage, err = store.RevisionCoverageForBatch(ctx, batch.ID)
	if err != nil || len(coverage) != 1 {
		t.Fatalf("coverage was not recorded: %v %v", coverage, err)
	}
	// Coverage has to identify which physical copy holds the batch, so a later
	// milestone can ask which retained copies contain a given revision.
	if coverage[0].DiskID != disk.ID || coverage[0].RepositoryID != repository.ID ||
		coverage[0].SnapshotID != fixtureSnapshotID {
		t.Fatalf("coverage does not identify the copy: %#v", coverage[0])
	}

	loadedRepository, _ := store.Repository(ctx, repository.ID)
	if loadedRepository.LastSnapshotID != fixtureSnapshotID {
		t.Fatalf("the repository's last snapshot was not recorded: %#v", loadedRepository)
	}
	loadedBackup, err := store.BackupRun(ctx, run.ID)
	if err != nil || loadedBackup == nil {
		t.Fatalf("load backup run: %v", err)
	}
	if loadedBackup.Stage != domain.StageDurableSuccess || !loadedBackup.DurableSuccess() {
		t.Fatalf("the completed backup is not durable: %#v", loadedBackup)
	}

	assertConsistent(t, store)
}

// The database itself refuses a durable success that cannot name its snapshot.
func TestDurableSuccessRequiresASnapshot(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)
	run := seedBackupRun(t, store, repository, nil)

	err := store.CompleteBackupRun(ctx, run, domain.BackupRun{
		RunID: run.ID, RepositoryID: repository.ID, DiskID: disk.ID,
		DurableSuccessAt: time.Now().UTC(),
	}, nil)
	if err == nil {
		t.Fatal("a durable success with no snapshot was accepted")
	}

	// And directly, so the constraint is proven rather than only the guard.
	_, err = store.db.ExecContext(ctx, `
		UPDATE backup_runs SET durability_stage = 'DURABLE_SUCCESS' WHERE run_id = ?`, run.ID)
	if err == nil {
		t.Fatal("the schema accepted a durable success with no verified snapshot")
	}
}

func TestBackupRunsAwaitingRecovery(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)

	// A run that never started restic leaves nothing behind to recover.
	early := seedBackupRun(t, store, repository, nil)
	if err := store.SetBackupStage(ctx, early.ID, domain.StageReadSetCaptured, time.Now().UTC()); err != nil {
		t.Fatalf("set stage: %v", err)
	}
	// A run that did start restic may have left a snapshot nobody confirmed.
	late := seedBackupRun(t, store, repository, nil)
	if err := store.SetBackupStage(ctx, late.ID, domain.StageResticRunning, time.Now().UTC()); err != nil {
		t.Fatalf("set stage: %v", err)
	}

	pending, err := store.BackupRunsAwaitingRecovery(ctx, repository.ID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].RunID != late.ID {
		t.Fatalf("pending recovery = %v, want only the run that started restic", pending)
	}
}

func TestActiveRepositoryRunsSelectsOnlyRepositoryKinds(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	source := seedSource(t, store, "documents", "documents")
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)

	if err := store.RecordRun(ctx, domain.Run{
		ID: mustID(t), Kind: domain.RunKindCollection, SourceID: source.ID,
		Status: domain.RunStatusRunning, RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record collection run: %v", err)
	}
	backupRun := seedBackupRun(t, store, repository, nil)

	active, err := store.ActiveRepositoryRuns(ctx)
	if err != nil {
		t.Fatalf("list active repository runs: %v", err)
	}
	if len(active) != 1 || active[0].ID != backupRun.ID {
		t.Fatalf("active repository runs = %v, want only the backup", active)
	}
}

// A candidate snapshot is written before verification precisely so that a crash
// in between leaves something for recovery to look for.
func TestCandidateSnapshotSurvivesWithoutVerification(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)
	run := seedBackupRun(t, store, repository, nil)

	if err := store.SetBackupStage(ctx, run.ID, domain.StageResticRunning, time.Now().UTC()); err != nil {
		t.Fatalf("set stage: %v", err)
	}
	if err := store.RecordCandidateSnapshot(ctx, run.ID, fixtureSnapshotID, domain.BackupRun{
		FilesProcessed: 11, BytesProcessed: 4096, BytesAdded: 2048,
	}); err != nil {
		t.Fatalf("record candidate: %v", err)
	}

	loaded, err := store.BackupRun(ctx, run.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load backup run: %v", err)
	}
	if loaded.CandidateSnapshotID != fixtureSnapshotID {
		t.Fatalf("candidate snapshot = %q", loaded.CandidateSnapshotID)
	}
	if loaded.VerifiedSnapshotID != "" {
		t.Fatal("a candidate snapshot was recorded as verified")
	}
	if !loaded.CandidateForRecovery() {
		t.Fatal("a run with an unverified candidate is not marked for recovery")
	}
	if loaded.FilesProcessed != 11 || loaded.BytesAdded != 2048 {
		t.Fatalf("counters were not persisted: %#v", loaded)
	}
}

// The schema must have no column that could hold a device node as identity.
func TestNoDeviceNodeIdentityInRepositorySchema(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	for _, table := range []string{"repositories", "backup_runs", "revision_coverage"} {
		rows, err := store.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("read %s columns: %v", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("scan column: %v", err)
			}
			switch name {
			case "device_path", "device_node", "mount_point", "device":
				rows.Close()
				t.Fatalf("table %s has column %q, which would persist an observation as identity", table, name)
			}
		}
		rows.Close()
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	return id
}
