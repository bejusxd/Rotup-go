package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestCompletionTransactionRefusesUnprovenDurabilityAndForeignCoverage(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	disk := seedDisk(t, store, "disk-a", "aaaaaaaa-1111-2222-3333-444444444444")
	repository := seedRepository(t, store, disk)
	run := seedBackupRun(t, store, repository, nil)
	run.Status = domain.RunStatusSucceeded
	at := time.Now().UTC()
	completed := domain.BackupRun{RunID: run.ID, RepositoryID: repository.ID, DiskID: disk.ID,
		VerifiedSnapshotID: fixtureSnapshotID, Durability: domain.DurabilityNormal, DurableSuccessAt: at}
	if err := store.CompleteBackupRun(context.Background(), run, completed, nil); err == nil {
		t.Fatal("success without sync/unmount evidence was accepted")
	}
	completed.SyncedAt, completed.UnmountVerifiedAt = at, at
	if err := store.CompleteBackupRun(context.Background(), run, completed, []domain.RevisionCoverage{{
		BackupRunID: run.ID, RepositoryID: repository.ID, DiskID: disk.ID, SnapshotID: fixtureSnapshotID,
		RevisionBatchID: "not-in-this-read-set", CoveredAt: at,
	}}); err == nil {
		t.Fatal("coverage for a batch outside the recorded read-set was accepted")
	}
	backup, err := store.BackupRun(context.Background(), run.ID)
	if err != nil || backup.Stage == domain.StageDurableSuccess {
		t.Fatal("a rejected coverage write failed to roll back the completion transaction")
	}
	loaded, err := store.Repository(context.Background(), repository.ID)
	if err != nil || loaded.LastSnapshotID != "" {
		t.Fatal("a rejected completion advanced repository history")
	}
}

func TestCancelledRebuildRestoresForeignKeys(t *testing.T) {
	t.Parallel()
	store := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := store.applyMigration(ctx, migration{version: 999, name: "cancelled-rebuild", rebuildsReferencedTable: true,
		sql: `WITH RECURSIVE forever(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM forever) SELECT sum(n) FROM forever;`})
	if err == nil {
		t.Fatal("an interrupted table rebuild succeeded")
	}
	var enabled int
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign key enforcement was lost after cancellation: %d / %v", enabled, err)
	}
}
