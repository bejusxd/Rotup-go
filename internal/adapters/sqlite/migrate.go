package sqlite

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

type appliedMigration struct {
	name     string
	checksum string
}

// Migrate applies every embedded migration exactly once and validates checksums.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			checksum   TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return 0, fmt.Errorf("create migration ledger: %w", err)
	}

	migrations, err := readMigrations()
	if err != nil {
		return 0, err
	}
	applied, err := s.readAppliedMigrations(ctx)
	if err != nil {
		return 0, err
	}

	embedded := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		embedded[item.version] = item
	}
	for version := range applied {
		if _, ok := embedded[version]; !ok {
			return 0, fmt.Errorf("database migration %d is newer than this binary", version)
		}
	}

	appliedCount := 0
	for _, item := range migrations {
		if existing, ok := applied[item.version]; ok {
			if existing.name != item.name || existing.checksum != item.checksum {
				return appliedCount, fmt.Errorf("migration %03d checksum or name mismatch", item.version)
			}
			continue
		}
		if err := s.applyMigration(ctx, item); err != nil {
			return appliedCount, err
		}
		appliedCount++
		s.logger.Info("SQLite migration applied", "version", item.version, "name", item.name)
	}

	s.logger.Info("SQLite migrations complete", "applied", appliedCount, "available", len(migrations))
	return appliedCount, nil
}

func readMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	result := make([]migration, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(entry.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, parseErr := strconv.Atoi(parts[0])
		if parseErr != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if _, duplicate := seen[version]; duplicate {
			return nil, fmt.Errorf("duplicate migration version %d", version)
		}
		seen[version] = struct{}{}

		contents, readErr := migrationFiles.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), readErr)
		}
		digest := sha256.Sum256(contents)
		result = append(result, migration{
			version:  version,
			name:     entry.Name(),
			checksum: hex.EncodeToString(digest[:]),
			sql:      string(contents),
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}

func (s *Store) readAppliedMigrations(ctx context.Context) (map[int]appliedMigration, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()

	result := make(map[int]appliedMigration)
	for rows.Next() {
		var version int
		var item appliedMigration
		if err := rows.Scan(&version, &item.name, &item.checksum); err != nil {
			return nil, fmt.Errorf("scan migration ledger: %w", err)
		}
		result[version] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration ledger: %w", err)
	}
	return result, nil
}

func (s *Store) applyMigration(ctx context.Context, item migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %03d: %w", item.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, item.sql); err != nil {
		return fmt.Errorf("execute migration %03d: %w", item.version, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_migrations (version, name, checksum, applied_at)
		VALUES (?, ?, ?, ?)`,
		item.version,
		item.name,
		item.checksum,
		time.Now().UTC().Format(timestampFormat),
	); err != nil {
		return fmt.Errorf("record migration %03d: %w", item.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %03d: %w", item.version, err)
	}
	return nil
}
