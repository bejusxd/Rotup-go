package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// RunKind identifies what a durable run was asked to do.
type RunKind string

const (
	// RunKindDemo is the M1 walking-skeleton record. It is retained so that
	// existing databases keep migrating and rendering.
	RunKindDemo RunKind = "DEMO"
	// RunKindConnectionTest is a read-only reachability probe for one source.
	RunKindConnectionTest RunKind = "CONNECTION_TEST"
	// RunKindCollection is a source collection into staging.
	RunKindCollection RunKind = "COLLECTION"
)

// RunTrigger records why a run exists. M2 has no scheduler, so runs are either
// requested by an administrator or created by startup recovery.
type RunTrigger string

const (
	RunTriggerManual   RunTrigger = "MANUAL"
	RunTriggerRecovery RunTrigger = "RECOVERY"
)

// RunStatus is the durable state of a run. M2 uses the subset of the
// architecture's statuses that manual operations can actually reach.
type RunStatus string

const (
	RunStatusPending              RunStatus = "PENDING"
	RunStatusRunning              RunStatus = "RUNNING"
	RunStatusSucceeded            RunStatus = "SUCCEEDED"
	RunStatusSucceededWithWarning RunStatus = "SUCCEEDED_WITH_WARNING"
	RunStatusPartial              RunStatus = "PARTIAL"
	RunStatusFailed               RunStatus = "FAILED"
	RunStatusInterrupted          RunStatus = "INTERRUPTED"
)

// Terminal reports whether the run has finished and will not change again.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunStatusPending, RunStatusRunning:
		return false
	default:
		return true
	}
}

// Successful reports whether the run achieved its goal. A run that only
// succeeded with warnings still counts; a partial or failed run does not.
func (s RunStatus) Successful() bool {
	return s == RunStatusSucceeded || s == RunStatusSucceededWithWarning
}

// Run is one durable execution request plus its summarized outcome.
type Run struct {
	ID          string
	Kind        RunKind
	TriggerKind RunTrigger
	SourceID    string
	Status      RunStatus
	Summary     string
	SummaryJSON string
	ErrorClass  ErrorClass
	ErrorCode   string
	SafeError   string
	RequestedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	UpdatedAt   time.Time
}

// Duration returns how long the run took, or zero while it is unfinished.
func (r Run) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// SourceResult is the per-source outcome of one collection run.
type SourceResult struct {
	RunID                  string
	SourceID               string
	Status                 RunStatus
	StagingStateVersion    int64
	PublishedAt            time.Time
	StartedAt              time.Time
	FinishedAt             time.Time
	BytesScanned           int64
	BytesCopied            int64
	FilesScanned           int64
	FilesCopied            int64
	FilesDeletedOrArchived int64
	EngineChecks           int64
	EngineErrors           int64
	Warnings               []string
}

// RevisionBatch records one run/source group of replaced or deleted staging
// objects. M2 creates batches but never expires them; retention is M8.
type RevisionBatch struct {
	ID                      string
	SourceID                string
	CollectionRunID         string
	RelativePath            string
	CreatedAt               time.Time
	ItemCount               int64
	ByteCount               int64
	ReconciliationCompleted bool
}

// Event severities for the append-only operational event log.
const (
	SeverityInfo     = "INFO"
	SeverityWarning  = "WARNING"
	SeverityError    = "ERROR"
	SeverityCritical = "CRITICAL"
)

// Event is an append-only administrator-facing operational fact.
type Event struct {
	ID          int64
	OccurredAt  time.Time
	Severity    string
	EventType   string
	Component   string
	SourceID    string
	RunID       string
	PayloadJSON string
}

// NewID returns a random UUID suitable for durable identifiers.
func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// IsID reports whether value looks like an identifier ROTUP generated. It is a
// cheap guard used before an identifier is turned into a filesystem path.
func IsID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
