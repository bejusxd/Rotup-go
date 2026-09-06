package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// migration 005 adds scheduling tables and widens runs. The thing worth proving
// is not that the SQL runs — it is that a database with real backup history
// comes out the other side with every row, value and reference intact, because
// the rebuild drops and recreates a table five other tables point into.

// m4Fixture is a populated M4-shaped database: the tables that reference runs
// all hold rows, so a rebuild that mishandles them cannot pass unnoticed.
type m4Fixture struct {
	path            string
	sourceID        string
	diskID          string
	repositoryID    string
	collectionRunID string
	backupRunID     string
	demoRunID       string
	pendingRunID    string
	batchID         string
	snapshotID      string
	at              time.Time
}

func buildM4Database(t *testing.T) m4Fixture {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "rotup.db")
	at := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)

	all, err := readMigrations()
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if len(all) < 5 {
		t.Fatalf("expected at least 5 migrations, found %d", len(all))
	}

	store, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL,
			checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	for _, item := range all[:4] {
		if err := store.applyMigration(ctx, item); err != nil {
			t.Fatalf("apply migration %03d: %v", item.version, err)
		}
	}

	f := m4Fixture{
		path:            path,
		collectionRunID: "00000000-0000-4000-8000-00000000c001",
		backupRunID:     "00000000-0000-4000-8000-00000000b001",
		demoRunID:       "00000000-0000-4000-8000-00000000d001",
		pendingRunID:    "00000000-0000-4000-8000-00000000p001",
		batchID:         "00000000-0000-4000-8000-0000000000b1",
		snapshotID:      "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00",
		at:              at,
	}
	source := seedSource(t, store, "documents", "documents")
	disk := seedDisk(t, store, "disk-a", "11111111-2222-3333-4444-555555555555")
	repository := seedRepository(t, store, disk)
	f.sourceID, f.diskID, f.repositoryID = source.ID, disk.ID, repository.ID

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, query)
		}
	}
	stamp := formatTime(at)

	// A demo run from M1, a finished collection, a finished backup, and one run
	// left PENDING. The pending row matters: it is the only active status an M4
	// database can hold, and it must still be active afterwards.
	exec(`INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, started_at, finished_at, updated_at)
		VALUES (?, 'DEMO', 'MANUAL', 'SUCCEEDED', 'demo', '{}', ?, ?, ?, ?)`,
		f.demoRunID, stamp, stamp, stamp, stamp)
	exec(`INSERT INTO runs (id, kind, trigger_kind, source_id, status, summary, summary_json,
			last_error_class, last_error_code, last_safe_error,
			requested_at, started_at, finished_at, updated_at)
		VALUES (?, 'COLLECTION', 'MANUAL', ?, 'SUCCEEDED_WITH_WARNING', 'collected 10 files',
			'{"files":10}', 'EXTERNAL_TOOL', 'rclone_notice', 'one object was skipped', ?, ?, ?, ?)`,
		f.collectionRunID, f.sourceID, stamp, stamp, stamp, stamp)
	exec(`INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, started_at, finished_at, updated_at)
		VALUES (?, 'BACKUP_LATEST', 'MANUAL', 'SUCCEEDED', 'snapshot c0ffee00', '{}', ?, ?, ?, ?)`,
		f.backupRunID, stamp, stamp, stamp, stamp)
	exec(`INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, updated_at)
		VALUES (?, 'VERIFY', 'RECOVERY', 'PENDING', '', '{}', ?, ?)`,
		f.pendingRunID, stamp, stamp)

	exec(`INSERT INTO run_source_results (collection_run_id, source_id, status,
			staging_state_version, published_at, started_at, finished_at,
			files_scanned, files_copied, bytes_scanned, bytes_copied,
			files_deleted_or_archived, engine_checks, engine_errors, warnings_json)
		VALUES (?, ?, 'SUCCEEDED', 3, ?, ?, ?, 10, 10, 2048, 2048, 2, 10, 0, '[]')`,
		f.collectionRunID, f.sourceID, stamp, stamp, stamp)
	exec(`INSERT INTO revision_batches (id, source_id, collection_run_id, relative_path,
			created_at, item_count, byte_count, reconciliation_completed)
		VALUES (?, ?, ?, ?, ?, 4, 2048, 1)`,
		f.batchID, f.sourceID, f.collectionRunID,
		"revisions/"+f.sourceID+"/"+f.collectionRunID, stamp)
	exec(`INSERT INTO backup_runs (run_id, repository_id, disk_id, durability_stage, durability,
			selected_source_ids_json, restic_started_at, candidate_snapshot_id,
			verified_snapshot_id, snapshot_verified_at, synced_at, unmount_verified_at,
			durable_success_at, files_processed, bytes_processed, bytes_added)
		VALUES (?, ?, ?, 'DURABLE_SUCCESS', 'NORMAL', ?, ?, ?, ?, ?, ?, ?, ?, 11, 4096, 2048)`,
		f.backupRunID, f.repositoryID, f.diskID, `["`+f.sourceID+`"]`,
		stamp, f.snapshotID, f.snapshotID, stamp, stamp, stamp, stamp)
	exec(`INSERT INTO backup_source_states (backup_run_id, source_id, current_collection_run_id,
			staging_state_version, published_at, current_relative_path, captured_at)
		VALUES (?, ?, ?, 3, ?, ?, ?)`,
		f.backupRunID, f.sourceID, f.collectionRunID, stamp,
		"sources/documents/current", stamp)
	exec(`INSERT INTO backup_revision_batches (backup_run_id, revision_batch_id, source_id,
			relative_path, batch_created_at, captured_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.backupRunID, f.batchID, f.sourceID,
		"revisions/"+f.sourceID+"/"+f.collectionRunID, stamp, stamp)
	exec(`INSERT INTO revision_coverage (revision_batch_id, backup_run_id, repository_id,
			disk_id, snapshot_id, covered_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.batchID, f.backupRunID, f.repositoryID, f.diskID, f.snapshotID, stamp)
	exec(`INSERT INTO events (occurred_at, severity, event_type, component,
			source_id, disk_id, run_id, repository_id, payload_json)
		VALUES (?, 'INFO', 'backup.snapshot_verified', 'backup', ?, ?, ?, ?, '{"ok":true}')`,
		stamp, f.sourceID, f.diskID, f.backupRunID, f.repositoryID)

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return f
}

func reopen(t *testing.T, path string) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := Open(context.Background(), path, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// scalar reads one value, failing the test rather than returning an error.
func scalar[T any](t *testing.T, store *Store, query string, args ...any) T {
	t.Helper()
	var value T
	if err := store.db.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func TestMigration005PreservesAPopulatedBackupHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := buildM4Database(t)
	store := reopen(t, f.path)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate an M4 database: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied %d migrations, want 1", applied)
	}

	// Nothing is orphaned: the rebuild dropped and recreated a table that
	// run_source_results, revision_batches, backup_runs, backup_source_states
	// and events all reference.
	if violations := scalar[int](t, store,
		`SELECT COUNT(*) FROM pragma_foreign_key_check`); violations != 0 {
		t.Fatalf("foreign_key_check reported %d violation(s)", violations)
	}
	if on := scalar[int](t, store, `PRAGMA foreign_keys`); on != 1 {
		t.Fatal("foreign key enforcement was not restored after the rebuild")
	}

	// Every run survived, with its values.
	if count := scalar[int](t, store, `SELECT COUNT(*) FROM runs`); count != 4 {
		t.Fatalf("runs after migration = %d, want 4", count)
	}
	collection, err := store.Run(ctx, f.collectionRunID)
	if err != nil || collection == nil {
		t.Fatalf("collection run lost: %v", err)
	}
	if collection.SourceID != f.sourceID {
		t.Fatalf("collection run lost its source: %q", collection.SourceID)
	}
	if collection.Summary != "collected 10 files" || collection.SummaryJSON != `{"files":10}` {
		t.Fatalf("collection summary changed: %q / %q", collection.Summary, collection.SummaryJSON)
	}
	if collection.SafeError != "one object was skipped" || collection.ErrorCode != "rclone_notice" {
		t.Fatalf("collection error detail changed: %#v", collection)
	}
	if !collection.RequestedAt.Equal(f.at) || !collection.FinishedAt.Equal(f.at) {
		t.Fatalf("collection timestamps changed: %#v", collection)
	}
	if demo, err := store.Run(ctx, f.demoRunID); err != nil || demo == nil {
		t.Fatalf("the M1 demo run did not survive: %v", err)
	}

	// A PENDING run is still PENDING and still active. Migration must not
	// resolve, sweep or reclassify anything.
	pending, err := store.Run(ctx, f.pendingRunID)
	if err != nil || pending == nil {
		t.Fatalf("pending run lost: %v", err)
	}
	if pending.Status != "PENDING" || pending.Status.Terminal() {
		t.Fatalf("pending run was altered: %s", pending.Status)
	}

	// The backup and everything that points at it.
	backup, err := store.BackupRun(ctx, f.backupRunID)
	if err != nil || backup == nil {
		t.Fatalf("backup run lost: %v", err)
	}
	if backup.VerifiedSnapshotID != f.snapshotID || backup.Stage != "DURABLE_SUCCESS" {
		t.Fatalf("backup run changed: %#v", backup)
	}
	if backup.BytesProcessed != 4096 || backup.FilesProcessed != 11 {
		t.Fatalf("backup counters changed: %#v", backup)
	}
	states, err := store.BackupSourceStates(ctx, f.backupRunID)
	if err != nil || len(states) != 1 || states[0].CurrentCollectionRunID != f.collectionRunID {
		t.Fatalf("read-set lost its link to the collection run: %v / %#v", err, states)
	}
	batches, err := store.BackupRevisionBatches(ctx, f.backupRunID)
	if err != nil || len(batches) != 1 || batches[0].RevisionBatchID != f.batchID {
		t.Fatalf("read-set revision batches lost: %v / %#v", err, batches)
	}
	if covered := scalar[int](t, store,
		`SELECT COUNT(*) FROM revision_coverage WHERE revision_batch_id = ? AND backup_run_id = ?`,
		f.batchID, f.backupRunID); covered != 1 {
		t.Fatal("proven revision coverage did not survive the rebuild")
	}
	if snapshot := scalar[string](t, store,
		`SELECT snapshot_id FROM revision_coverage WHERE revision_batch_id = ?`,
		f.batchID); snapshot != f.snapshotID {
		t.Fatalf("coverage snapshot changed: %s", snapshot)
	}

	// The event still points at its run and repository.
	if linked := scalar[int](t, store,
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND repository_id = ? AND source_id = ?`,
		f.backupRunID, f.repositoryID, f.sourceID); linked != 1 {
		t.Fatal("the event lost one of its references")
	}
	if result := scalar[int](t, store,
		`SELECT COUNT(*) FROM run_source_results
		 WHERE collection_run_id = ? AND source_id = ? AND staging_state_version = 3`,
		f.collectionRunID, f.sourceID); result != 1 {
		t.Fatal("the collection's source result did not survive")
	}

	// New columns exist and default to "not scheduled, never attempted".
	if jobs := scalar[int](t, store, `SELECT COUNT(*) FROM runs WHERE job_id IS NOT NULL`); jobs != 0 {
		t.Fatal("migrated runs were given a job")
	}
	if keys := scalar[int](t, store,
		`SELECT COUNT(*) FROM runs WHERE idempotency_key IS NOT NULL`); keys != 0 {
		t.Fatal("migrated runs were given idempotency keys")
	}
	if attempts := scalar[int](t, store,
		`SELECT COUNT(*) FROM runs WHERE attempt_count <> 0 OR priority <> 0`); attempts != 0 {
		t.Fatal("migrated runs were given retry or priority state")
	}

	// Automation is inert: the scheduling tables exist and are empty.
	for _, table := range []string{"jobs", "schedules", "job_sources"} {
		if count := scalar[int](t, store, `SELECT COUNT(*) FROM `+table); count != 0 {
			t.Fatalf("%s is not empty after migration: %d rows", table, count)
		}
	}

	// Re-opening and migrating again changes nothing.
	if again, err := store.Migrate(ctx); err != nil || again != 0 {
		t.Fatalf("second migrate applied %d migrations: %v", again, err)
	}
}

// A rebuild that would leave a dangling reference must abort and leave the
// database exactly as it was, not commit a half-migrated schema.
func TestMigration005IsAtomicWhenTheRebuildWouldOrphanARow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := buildM4Database(t)
	store := reopen(t, f.path)

	// Plant a run pointing at a source that does not exist. Foreign keys have to
	// be off to create it, which is exactly the corruption foreign_key_check
	// inside the migration exists to catch.
	if _, err := store.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, source_id, status, summary, summary_json,
			requested_at, updated_at)
		VALUES ('bad-run', 'COLLECTION', 'MANUAL', 'no-such-source', 'PENDING', '', '{}', ?, ?)`,
		formatTime(f.at), formatTime(f.at)); err != nil {
		t.Fatalf("plant the dangling run: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable foreign keys: %v", err)
	}

	if _, err := store.Migrate(ctx); err == nil {
		t.Fatal("the migration committed a referentially inconsistent rebuild")
	}

	// Version 5 was not recorded, and the old table is still in place with its
	// data: the failure rolled the whole thing back.
	if recorded := scalar[int](t, store,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 5`); recorded != 0 {
		t.Fatal("a failed migration was recorded as applied")
	}
	if columns := scalar[int](t, store,
		`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'job_id'`); columns != 0 {
		t.Fatal("a failed migration left the widened runs table behind")
	}
	if count := scalar[int](t, store, `SELECT COUNT(*) FROM runs`); count != 5 {
		t.Fatalf("runs after the failed migration = %d, want 5", count)
	}
	if backup := scalar[int](t, store,
		`SELECT COUNT(*) FROM backup_runs WHERE run_id = ?`, f.backupRunID); backup != 1 {
		t.Fatal("the failed migration damaged backup history")
	}
	if on := scalar[int](t, store, `PRAGMA foreign_keys`); on != 1 {
		t.Fatal("foreign key enforcement was not restored after the failed migration")
	}
}

// The new columns are constrained, not merely present.
func TestMigration005ConstrainsTheNewSchedulingColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stamp := formatTime(time.Date(2026, time.September, 6, 9, 0, 0, 0, time.UTC))

	insertJob := func(id, name, kind string, timeout int) error {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO jobs (id, name, kind, timeout_seconds, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, name, kind, timeout, stamp, stamp)
		return err
	}
	if err := insertJob("j1", "nightly", "COLLECTION", 7200); err != nil {
		t.Fatalf("a valid job was rejected: %v", err)
	}
	if err := insertJob("j2", "nightly", "COLLECTION", 7200); err == nil {
		t.Fatal("a duplicate job name was accepted")
	}
	if err := insertJob("j3", "rotate", "ROTATION", 7200); err == nil {
		t.Fatal("an unknown job kind was accepted")
	}
	if err := insertJob("j4", "instant", "COLLECTION", 0); err == nil {
		t.Fatal("a zero timeout was accepted")
	}

	insertSchedule := func(id string, mask int) error {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO schedules (id, job_id, days_of_week_mask, local_time, timezone,
				created_at, updated_at)
			VALUES (?, 'j1', ?, '02:30', 'Europe/Warsaw', ?, ?)`, id, mask, stamp, stamp)
		return err
	}
	if err := insertSchedule("s1", 0b0111110); err != nil {
		t.Fatalf("a valid schedule was rejected: %v", err)
	}
	if err := insertSchedule("s2", 0); err == nil {
		t.Fatal("a schedule selecting no weekday was accepted")
	}
	if err := insertSchedule("s3", 128); err == nil {
		t.Fatal("a schedule with an out-of-range weekday mask was accepted")
	}

	// Deleting the job takes its schedules with it, but a source a job still
	// references may not simply vanish.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = 'j1'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if left := scalar[int](t, store, `SELECT COUNT(*) FROM schedules`); left != 0 {
		t.Fatalf("%d schedule(s) outlived their job", left)
	}
}

// Idempotency keys are what stop one nominal due time producing two runs, and
// they must not get in the way of manual runs, which carry none.
func TestMigration005EnforcesIdempotencyKeysWithoutBlockingManualRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stamp := formatTime(time.Date(2026, time.September, 6, 9, 0, 0, 0, time.UTC))

	insert := func(id string, key any) error {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO runs (id, kind, trigger_kind, status, idempotency_key,
				summary, summary_json, requested_at, updated_at)
			VALUES (?, 'COLLECTION', 'MANUAL', 'PENDING', ?, '', '{}', ?, ?)`,
			id, key, stamp, stamp)
		return err
	}
	if err := insert("r1", "job:nightly@2026-09-06T02:30"); err != nil {
		t.Fatalf("first keyed run rejected: %v", err)
	}
	if err := insert("r2", "job:nightly@2026-09-06T02:30"); err == nil {
		t.Fatal("a duplicate idempotency key was accepted")
	}
	if err := insert("r3", nil); err != nil {
		t.Fatalf("first manual run rejected: %v", err)
	}
	if err := insert("r4", nil); err != nil {
		t.Fatalf("a second manual run was blocked by the unique key: %v", err)
	}

	// The new statuses and triggers are accepted; nonsense still is not.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			waiting_reason, requested_at, updated_at)
		VALUES ('r5', 'BACKUP_LATEST', 'SCHEDULED', 'WAITING_FOR_RESOURCE', '', '{}',
			'no registered disk is connected', ?, ?)`, stamp, stamp); err != nil {
		t.Fatalf("a scheduled waiting run was rejected: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, updated_at)
		VALUES ('r6', 'COLLECTION', 'MANUAL', 'ALMOST_DONE', '', '{}', ?, ?)`,
		stamp, stamp); err == nil {
		t.Fatal("an unknown run status was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, trigger_kind, status, summary, summary_json,
			requested_at, updated_at)
		VALUES ('r7', 'COLLECTION', 'CRON', 'PENDING', '', '{}', ?, ?)`,
		stamp, stamp); err == nil {
		t.Fatal("an unknown trigger kind was accepted")
	}
}
