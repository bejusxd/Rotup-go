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
	// RunKindDiskSetup is an explicit restic repository initialization or
	// adoption on one registered disk.
	RunKindDiskSetup RunKind = "DISK_SETUP"
	// RunKindBackupLatest is a snapshot of whatever stable staging state is
	// current when the run acquires the staging barrier.
	RunKindBackupLatest RunKind = "BACKUP_LATEST"
	// RunKindVerify is a non-destructive repository verification, used by the
	// explicit recovery workflow.
	RunKindVerify RunKind = "VERIFY"
	// RunKindRetention is scheduled retention work. The schema accepts it from
	// M5 onwards because the architecture defines it, but no code produces one:
	// retention is M8.
	RunKindRetention RunKind = "RETENTION"
)

// TouchesRepository reports whether a run kind operates on a restic repository.
// Startup recovery uses it to decide which unfinished runs it owns.
// RETENTION is included even though nothing produces one yet. Retention runs
// forget and prune inside a repository, so an interrupted one leaves exactly the
// ambiguity this classification exists to catch. Declaring it here means M8
// cannot start writing such runs while the sweep quietly ignores them.
func (k RunKind) TouchesRepository() bool {
	switch k {
	case RunKindDiskSetup, RunKindBackupLatest, RunKindVerify, RunKindRetention:
		return true
	default:
		return false
	}
}

// RunTrigger records why a run exists.
type RunTrigger string

const (
	// RunTriggerManual is an administrator's explicit request.
	RunTriggerManual RunTrigger = "MANUAL"
	// RunTriggerRecovery is created by startup or explicit recovery.
	RunTriggerRecovery RunTrigger = "RECOVERY"
	// The triggers below exist for the M5 orchestrator. The schema accepts them
	// from migration 005 onwards; no code produces one yet.
	//
	// RunTriggerScheduled is a schedule reaching its nominal due time.
	RunTriggerScheduled RunTrigger = "SCHEDULED"
	// RunTriggerCollectionSuccess is a backup chained after a collection that
	// published, when the job asks for it.
	RunTriggerCollectionSuccess RunTrigger = "COLLECTION_SUCCESS"
	// RunTriggerDeviceAvailable is work released by a disk becoming available.
	RunTriggerDeviceAvailable RunTrigger = "DEVICE_AVAILABLE"
	// RunTriggerRetry is a further attempt at a run that failed retryably.
	RunTriggerRetry RunTrigger = "RETRY"
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

	// The statuses below belong to the M5 orchestrator. Migration 005 accepts
	// them; no code writes one yet. They are declared now so that the
	// classification below is written once, deliberately, rather than being
	// discovered later by whichever slice first writes one.
	//
	// RunStatusWaitingForResource is a run whose disk, source or lock is not
	// available. Waiting is not failing and does not consume retry attempts.
	RunStatusWaitingForResource RunStatus = "WAITING_FOR_RESOURCE"
	// RunStatusQueued is due and admissible, awaiting a worker.
	RunStatusQueued RunStatus = "QUEUED"
	// RunStatusPreflight is a worker holding resources and running checks.
	RunStatusPreflight RunStatus = "PREFLIGHT"
	// RunStatusFinalizing is work completed, outcome not yet committed.
	RunStatusFinalizing RunStatus = "FINALIZING"
	// RunStatusRetryDelay is a retryable failure waiting for its interval.
	RunStatusRetryDelay RunStatus = "RETRY_DELAY"
	// RunStatusTimedOut is a run stopped by its own deadline.
	RunStatusTimedOut RunStatus = "TIMED_OUT"
	// RunStatusCanceled is a run an administrator stopped before it ran.
	RunStatusCanceled RunStatus = "CANCELED"
	// RunStatusSkipped is a due run deliberately not started, with a reason.
	RunStatusSkipped RunStatus = "SKIPPED"
	// RunStatusSuperseded is automatic demand safely coalesced into a later
	// run. It stays in history so the decision remains auditable.
	RunStatusSuperseded RunStatus = "SUPERSEDED"
)

// Terminal reports whether the run has finished and will not change again.
//
// This enumerates the *active* statuses and treats everything else as finished,
// with one deliberate exception: a value this build does not recognise is
// reported as active. The direction of that default is a safety property. A
// status wrongly called finished is a run nothing will ever sweep, which after a
// crash means a repository that is never marked suspect — the false-success shape
// M4 exists to prevent. A status wrongly called active is merely swept to
// INTERRUPTED, which is recoverable and visible.
//
// The startup sweep's SQL still lists PENDING and RUNNING literally, which is
// correct while those are the only active statuses anything writes. Deriving
// those queries from this classification belongs to the slice that first writes
// one of the new statuses, and must not lag behind it.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunStatusPending, RunStatusWaitingForResource, RunStatusQueued,
		RunStatusPreflight, RunStatusRunning, RunStatusFinalizing,
		RunStatusRetryDelay:
		return false
	case RunStatusSucceeded, RunStatusSucceededWithWarning, RunStatusPartial,
		RunStatusFailed, RunStatusTimedOut, RunStatusInterrupted,
		RunStatusCanceled, RunStatusSkipped, RunStatusSuperseded:
		return true
	default:
		return false
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
	ID           int64
	OccurredAt   time.Time
	Severity     string
	EventType    string
	Component    string
	SourceID     string
	DiskID       string
	RepositoryID string
	RunID        string
	PayloadJSON  string
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
