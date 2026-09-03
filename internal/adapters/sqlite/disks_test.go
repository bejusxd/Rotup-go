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

func seedDisk(t *testing.T, store *Store, name, uuid string) domain.Disk {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	now := time.Now().UTC().Round(time.Millisecond)
	disk := domain.Disk{
		ID: id, Name: name, FilesystemUUID: uuid, FilesystemType: "ext4",
		Enabled: true, FirstRegisteredAt: now,
		CurrentStatus: domain.DiskAvailableUnmounted, UpdatedAt: now,
	}
	if err := store.CreateDisk(context.Background(), disk); err != nil {
		t.Fatalf("create disk: %v", err)
	}
	loaded, err := store.Disk(context.Background(), id)
	if err != nil || loaded == nil {
		t.Fatalf("load disk: %v", err)
	}
	return *loaded
}

// An existing M2 database must upgrade in place with its sources, runs and
// staging history intact.
func TestExistingM2DatabaseUpgradesAndKeepsItsHistory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rotup.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	migrations, err := readMigrations()
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if len(migrations) < 3 {
		t.Fatalf("expected at least three migrations, found %d", len(migrations))
	}

	// Build a database with only migrations 001 and 002 applied.
	first, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := first.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL,
			checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	for _, migration := range migrations[:2] {
		if err := first.applyMigration(ctx, migration); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
	}

	source := seedSource(t, first, "documents", "documents")
	runID, _ := domain.NewID()
	if err := first.RecordRun(ctx, domain.Run{
		ID: runID, Kind: domain.RunKindCollection, SourceID: source.ID,
		Status: domain.RunStatusSucceeded, Summary: "collected 3 files",
		RequestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
	// Written with the M2-era column set, because that is all this database has
	// at this point; the current repository already knows about disk_id.
	if _, err := first.db.ExecContext(ctx, `
		INSERT INTO events (occurred_at, severity, event_type, component, source_id, payload_json)
		VALUES (?, 'INFO', 'source.created', 'sources', ?, '{}')`,
		formatTime(time.Now().UTC()), source.ID); err != nil {
		t.Fatalf("append M2-era event: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening applies only the new migration.
	store, err := Open(ctx, path, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade an M2 database: %v", err)
	}
	if applied != len(migrations)-2 {
		t.Fatalf("applied %d migrations, want %d", applied, len(migrations)-2)
	}

	// The M2 history is untouched.
	loadedSource, err := store.Source(ctx, source.ID)
	if err != nil || loadedSource == nil {
		t.Fatalf("the source did not survive the upgrade: %v", err)
	}
	if loadedSource.Name != "documents" {
		t.Fatalf("source = %#v", loadedSource)
	}
	run, err := store.Run(ctx, runID)
	if err != nil || run == nil {
		t.Fatalf("the run did not survive the upgrade: %v", err)
	}
	if run.Summary != "collected 3 files" {
		t.Fatalf("run summary = %q", run.Summary)
	}
	events, err := store.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].SourceID != source.ID {
		t.Fatalf("the event history did not survive: %#v", events)
	}
	if events[0].DiskID != "" {
		t.Fatal("a pre-existing event must not gain a disk association")
	}

	// The new schema works.
	disk := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")
	if err := store.AppendEvent(ctx, domain.Event{
		OccurredAt: time.Now().UTC(), Severity: domain.SeverityInfo,
		EventType: "disk.registered", Component: "removable", DiskID: disk.ID,
	}); err != nil {
		t.Fatalf("append disk event: %v", err)
	}
	all, err := store.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	diskEvents := 0
	for _, event := range all {
		if event.DiskID == disk.ID {
			diskEvents++
		}
	}
	if diskEvents != 1 {
		t.Fatalf("disk events = %d, want 1", diskEvents)
	}
	assertConsistent(t, store)
}

func TestDiskRegistrationPersistence(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")

	if disk.CurrentStatus != domain.DiskAvailableUnmounted || !disk.Enabled {
		t.Fatalf("disk = %#v", disk)
	}
	byUUID, err := store.DiskByFilesystemUUID(ctx, "11111111-2222-3333-4444-555555555555")
	if err != nil || byUUID == nil {
		t.Fatalf("look up by UUID: %v", err)
	}
	if byUUID.ID != disk.ID {
		t.Fatal("the UUID lookup returned the wrong disk")
	}
	// Identity comparison is case-insensitive, matching how filesystems report
	// their UUIDs inconsistently across tools.
	upper, err := store.DiskByFilesystemUUID(ctx, "11111111-2222-3333-4444-555555555555")
	if err != nil || upper == nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}

	missing, err := store.DiskByFilesystemUUID(ctx, "99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("look up an unknown UUID: %v", err)
	}
	if missing != nil {
		t.Fatal("an unknown UUID must read back as nil")
	}
	assertConsistent(t, store)
}

func TestDuplicateDiskIdentityIsRejectedBySchema(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	first := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")

	id, _ := domain.NewID()
	duplicate := first
	duplicate.ID = id
	duplicate.Name = "backup A duplicate"
	err := store.CreateDisk(context.Background(), duplicate)
	if err == nil {
		t.Fatal("a second disk with the same filesystem UUID must be refused")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "disk_already_registered" {
		t.Fatalf("error = %v, want disk_already_registered", err)
	}

	sameName := first
	sameName.ID = id
	sameName.FilesystemUUID = "99999999-9999-9999-9999-999999999999"
	err = store.CreateDisk(context.Background(), sameName)
	if err == nil {
		t.Fatal("a duplicate disk name must be refused")
	}
	if !domain.AsError(err, &typed) || typed.Code != "disk_name_taken" {
		t.Fatalf("error = %v, want disk_name_taken", err)
	}
}

func TestDiskStateTransitionsPersist(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")

	for _, status := range []domain.DiskStatus{
		domain.DiskValidating, domain.DiskMounting, domain.DiskReady,
		domain.DiskSyncing, domain.DiskUnmounting, domain.DiskSafeToRemove,
		domain.DiskAvailableUnmounted, domain.DiskDetected, domain.DiskError,
		domain.DiskDisconnected,
	} {
		if err := store.SaveDiskState(ctx, domain.DiskState{ID: disk.ID, Status: status}); err != nil {
			t.Fatalf("save state %s: %v", status, err)
		}
		loaded, err := store.Disk(ctx, disk.ID)
		if err != nil || loaded == nil {
			t.Fatalf("reload disk: %v", err)
		}
		if loaded.CurrentStatus != status {
			t.Fatalf("status = %s, want %s", loaded.CurrentStatus, status)
		}
	}

	// The schema refuses a status ROTUP does not define.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE disks SET current_status = 'PROBABLY_FINE' WHERE id = ?`, disk.ID); err == nil {
		t.Fatal("the schema must reject an undefined disk status")
	}
}

func TestConnectionTimestampsAreOnlyWrittenWhenSupplied(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")
	connected := time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC)

	if err := store.SaveDiskState(ctx, domain.DiskState{
		ID: disk.ID, Status: domain.DiskAvailableUnmounted, LastConnectedAt: &connected,
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	// A later transition that says nothing about connectivity must leave the
	// recorded history alone rather than clearing it.
	if err := store.SaveDiskState(ctx, domain.DiskState{
		ID: disk.ID, Status: domain.DiskError, ErrorCode: "external_mount",
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	loaded, err := store.Disk(ctx, disk.ID)
	if err != nil || loaded == nil {
		t.Fatalf("reload disk: %v", err)
	}
	if loaded.LastConnectedAt == nil || !loaded.LastConnectedAt.Equal(connected) {
		t.Fatalf("last connected = %v, want it preserved", loaded.LastConnectedAt)
	}
	if loaded.LastErrorCode != "external_mount" {
		t.Fatalf("error code = %q", loaded.LastErrorCode)
	}
}

func TestUpdateDiskSettingsLeavesIdentityAlone(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	ctx := context.Background()
	disk := seedDisk(t, store, "backup A", "11111111-2222-3333-4444-555555555555")

	if err := store.UpdateDiskSettings(ctx, disk.ID, "renamed", false); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	loaded, err := store.Disk(ctx, disk.ID)
	if err != nil || loaded == nil {
		t.Fatalf("reload disk: %v", err)
	}
	if loaded.Name != "renamed" || loaded.Enabled {
		t.Fatalf("disk = %#v", loaded)
	}
	if loaded.FilesystemUUID != disk.FilesystemUUID || loaded.FilesystemType != disk.FilesystemType {
		t.Fatal("editing metadata must never change the disk's identity")
	}

	if err := store.UpdateDiskSettings(ctx, "99999999-9999-4999-8999-999999999999", "x", true); err == nil {
		t.Fatal("updating an unknown disk must fail")
	}
	assertConsistent(t, store)
}

// The schema must have no place to record a device node as identity.
func TestDiskSchemaStoresNoDeviceNode(t *testing.T) {
	t.Parallel()

	store := migratedStore(t)
	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(disks)`)
	if err != nil {
		t.Fatalf("read table info: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var index int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&index, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		for _, forbidden := range []string{"device", "dev_path", "devnode", "device_path", "path"} {
			if name == forbidden {
				t.Fatalf("the disks table has a %q column; a device node must never be durable identity", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table info: %v", err)
	}
}
