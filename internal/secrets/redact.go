package secrets

import (
	"os"
	"strings"
)

// Placeholder replaces any secret value found in captured tool output.
const Placeholder = "[REDACTED]"

// Redactor removes known secret values from text that is about to be logged,
// persisted, or rendered. It is defence in depth: ROTUP never passes secrets in
// arguments, but an external tool may still echo material it was given.
type Redactor struct {
	values []string
}

// NewRedactor builds a redactor for the given secret values. Empty and very
// short values are ignored because replacing them would corrupt diagnostics
// without protecting anything meaningful.
func NewRedactor(values ...string) *Redactor {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) < 4 {
			continue
		}
		filtered = append(filtered, value)
	}
	return &Redactor{values: filtered}
}

// Add registers another value to redact.
func (r *Redactor) Add(value string) {
	if len(value) < 4 {
		return
	}
	r.values = append(r.values, value)
}

// Redact returns text with every registered secret value replaced.
func (r *Redactor) Redact(text string) string {
	if r == nil || len(r.values) == 0 {
		return text
	}
	for _, value := range r.values {
		text = strings.ReplaceAll(text, value, Placeholder)
	}
	return text
}

// syncDir best-effort flushes a directory entry so a rename is durable. It is a
// no-op on platforms where directories cannot be synced.
func syncDir(dir string) {
	file, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = file.Sync()
	_ = file.Close()
}
