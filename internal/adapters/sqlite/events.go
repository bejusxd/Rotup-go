package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bejusxd/rotup/internal/domain"
)

// maxEventPayload bounds a single event payload so the event log stays a
// control-state table rather than a log sink.
const maxEventPayload = 4096

// AppendEvent records one append-only operational fact.
func (s *Store) AppendEvent(ctx context.Context, event domain.Event) error {
	payload := event.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	if len(payload) > maxEventPayload {
		payload = `{"truncated":true}`
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO events (occurred_at, severity, event_type, component, source_id, run_id, payload_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			formatTime(event.OccurredAt),
			event.Severity,
			event.EventType,
			event.Component,
			nullableText(event.SourceID),
			nullableText(event.RunID),
			payload,
		)
		if err != nil {
			return fmt.Errorf("append event: %w", err)
		}
		return nil
	})
}

// ListEvents returns the most recent events, newest first.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, occurred_at, severity, event_type, component, source_id, run_id, payload_json
		FROM events
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var result []domain.Event
	for rows.Next() {
		var event domain.Event
		var occurredAt sql.NullString
		var sourceID, runID sql.NullString
		if err := rows.Scan(&event.ID, &occurredAt, &event.Severity, &event.EventType,
			&event.Component, &sourceID, &runID, &event.PayloadJSON); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.SourceID = sourceID.String
		event.RunID = runID.String
		if event.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return result, nil
}
