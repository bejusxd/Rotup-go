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

func TestMigrations(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	first, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("first migration run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first migration count = %d, want 1", first)
	}
	second, err := store.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	if second != 0 {
		t.Fatalf("second migration count = %d, want 0", second)
	}

	var foreignKeys int
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode pragma: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
}

func TestRunPersistenceAndReadback(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Round(0)
	want := domain.Run{
		ID:          "00000000-0000-4000-8000-000000000001",
		Kind:        domain.RunKindDemo,
		Status:      domain.RunStatusSucceeded,
		Summary:     "M1 demo run",
		RequestedAt: now,
		StartedAt:   now,
		FinishedAt:  now,
	}
	if err := store.RecordRun(context.Background(), want); err != nil {
		t.Fatalf("record run: %v", err)
	}

	got, err := store.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("latest run: %v", err)
	}
	if got == nil {
		t.Fatal("latest run is nil")
	}
	if *got != want {
		t.Fatalf("latest run = %#v, want %#v", *got, want)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "rotup.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}
