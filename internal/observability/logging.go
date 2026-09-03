package observability

import (
	"io"
	"log/slog"
)

// NewLogger creates the structured JSON logger used by the daemon.
func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}
