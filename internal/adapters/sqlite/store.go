package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	_ "modernc.org/sqlite"
)

const timestampFormat = time.RFC3339Nano

// Store owns the single M1 SQLite connection.
type Store struct {
	db        *sql.DB
	logger    *slog.Logger
	closeOnce sync.Once
	closeErr  error
}

// Open opens a SQLite database and applies connection safety settings.
func Open(ctx context.Context, path string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := ensureParentDirectory(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	// M1 uses one serialized connection. This also ensures connection-scoped
	// pragmas apply to every operation and keeps in-memory tests deterministic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, logger: logger}
	if err := store.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	logger.Info("SQLite initialized", "path", path)
	return store, nil
}

func ensureParentDirectory(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create SQLite directory %q: %w", directory, err)
	}
	return nil
}

func (s *Store) configure(ctx context.Context) error {
	statements := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite with %q: %w", statement, err)
		}
	}

	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable SQLite WAL mode: %w", err)
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("verify SQLite foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("SQLite foreign key enforcement is disabled")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite: %w", err)
	}

	s.logger.Debug("SQLite pragmas configured", "journal_mode", journalMode, "busy_timeout_ms", 5000)
	return nil
}

// RecordRun stores one complete M1 demo run.
func (s *Store) RecordRun(ctx context.Context, run domain.Run) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, kind, status, summary, requested_at, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.ID,
		run.Kind,
		run.Status,
		run.Summary,
		run.RequestedAt.UTC().Format(timestampFormat),
		run.StartedAt.UTC().Format(timestampFormat),
		run.FinishedAt.UTC().Format(timestampFormat),
	)
	if err != nil {
		return fmt.Errorf("record run %q: %w", run.ID, err)
	}
	return nil
}

// LatestRun returns the most recently finished M1 run, or nil when history is empty.
func (s *Store) LatestRun(ctx context.Context) (*domain.Run, error) {
	var run domain.Run
	var requestedAt, startedAt, finishedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, status, summary, requested_at, started_at, finished_at
		FROM runs
		ORDER BY finished_at DESC, id DESC
		LIMIT 1`).Scan(
		&run.ID,
		&run.Kind,
		&run.Status,
		&run.Summary,
		&requestedAt,
		&startedAt,
		&finishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest run: %w", err)
	}

	timestamps := []struct {
		value  string
		target *time.Time
	}{
		{value: requestedAt, target: &run.RequestedAt},
		{value: startedAt, target: &run.StartedAt},
		{value: finishedAt, target: &run.FinishedAt},
	}
	for _, timestamp := range timestamps {
		parsed, parseErr := time.Parse(timestampFormat, timestamp.value)
		if parseErr != nil {
			return nil, fmt.Errorf("parse run timestamp %q: %w", timestamp.value, parseErr)
		}
		*timestamp.target = parsed
	}

	return &run, nil
}

// Health verifies that SQLite can serve a simple query.
func (s *Store) Health(ctx context.Context) error {
	var value int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
		return fmt.Errorf("SQLite health query: %w", err)
	}
	if value != 1 {
		return fmt.Errorf("SQLite health query returned %d", value)
	}
	return nil
}

// Close releases the SQLite connection. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
