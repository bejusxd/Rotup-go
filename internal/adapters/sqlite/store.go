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

	_ "modernc.org/sqlite"
)

const timestampFormat = time.RFC3339Nano

// Store owns the single SQLite connection.
type Store struct {
	db        *sql.DB
	logger    *slog.Logger
	closeOnce sync.Once
	closeErr  error
	// writeMu serializes write transactions. The connection pool is already
	// limited to one connection; this keeps multi-statement write sequences
	// from interleaving with each other.
	writeMu sync.Mutex
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
	// One serialized connection. This also ensures connection-scoped pragmas
	// apply to every operation and keeps tests deterministic.
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

// ForeignKeyCheck reports rows that violate a foreign key constraint. An empty
// result means the database is referentially consistent.
func (s *Store) ForeignKeyCheck(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()

	var violations []string
	for rows.Next() {
		var table, parent sql.NullString
		var rowID, constraint sql.NullInt64
		if err := rows.Scan(&table, &rowID, &parent, &constraint); err != nil {
			return nil, fmt.Errorf("scan foreign key check: %w", err)
		}
		violations = append(violations,
			fmt.Sprintf("table %s row %d references %s", table.String, rowID.Int64, parent.String))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate foreign key check: %w", err)
	}
	return violations, nil
}

// IntegrityCheck runs SQLite's quick integrity check.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check reported %q", result)
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

// withWriteTx runs fn inside a single short write transaction. No filesystem,
// network, or subprocess work may happen inside fn.
func (s *Store) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// formatTime renders a UTC timestamp for storage.
func formatTime(value time.Time) string {
	return value.UTC().Format(timestampFormat)
}

// nullableTime renders an optional timestamp, storing NULL for the zero value.
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}

// nullableText stores NULL for an empty string so optional columns stay clean.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func parseTime(value sql.NullString) (time.Time, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(timestampFormat, value.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value.String, err)
	}
	return parsed, nil
}
