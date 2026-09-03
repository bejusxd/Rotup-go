package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bejusxd/rotup/internal/domain"
)

// RecordRevisionBatch stores one run/source group of archived staging objects.
// reconciliation_completed records whether the run that produced the batch
// finished its reconciliation, so an interrupted run's partial archive
// directory can never be mistaken for a finished batch.
func (s *Store) RecordRevisionBatch(ctx context.Context, batch domain.RevisionBatch) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO revision_batches (
				id, source_id, collection_run_id, relative_path,
				created_at, item_count, byte_count, reconciliation_completed)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (relative_path) DO UPDATE SET
				item_count = excluded.item_count,
				byte_count = excluded.byte_count,
				reconciliation_completed = excluded.reconciliation_completed`,
			batch.ID,
			batch.SourceID,
			batch.CollectionRunID,
			batch.RelativePath,
			formatTime(batch.CreatedAt),
			batch.ItemCount,
			batch.ByteCount,
			boolToInt(batch.ReconciliationCompleted),
		)
		if err != nil {
			return fmt.Errorf("record revision batch: %w", err)
		}
		return nil
	})
}

// ListRevisionBatches returns a source's revision batches, newest first.
func (s *Store) ListRevisionBatches(ctx context.Context, sourceID string, limit int) ([]domain.RevisionBatch, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_id, collection_run_id, relative_path,
		       created_at, item_count, byte_count, reconciliation_completed
		FROM revision_batches
		WHERE source_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list revision batches: %w", err)
	}
	defer rows.Close()

	var result []domain.RevisionBatch
	for rows.Next() {
		var batch domain.RevisionBatch
		var createdAt sql.NullString
		var completed int
		if err := rows.Scan(&batch.ID, &batch.SourceID, &batch.CollectionRunID,
			&batch.RelativePath, &createdAt, &batch.ItemCount, &batch.ByteCount, &completed); err != nil {
			return nil, fmt.Errorf("scan revision batch: %w", err)
		}
		batch.ReconciliationCompleted = completed != 0
		if batch.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		result = append(result, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate revision batches: %w", err)
	}
	return result, nil
}

// RevisionBatchForRun returns the batch a collection run produced, if any.
func (s *Store) RevisionBatchForRun(ctx context.Context, runID string) (*domain.RevisionBatch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source_id, collection_run_id, relative_path,
		       created_at, item_count, byte_count, reconciliation_completed
		FROM revision_batches
		WHERE collection_run_id = ?`, runID)

	var batch domain.RevisionBatch
	var createdAt sql.NullString
	var completed int
	err := row.Scan(&batch.ID, &batch.SourceID, &batch.CollectionRunID,
		&batch.RelativePath, &createdAt, &batch.ItemCount, &batch.ByteCount, &completed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read revision batch: %w", err)
	}
	batch.ReconciliationCompleted = completed != 0
	if batch.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &batch, nil
}
