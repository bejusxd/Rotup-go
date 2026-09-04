package ports

import (
	"context"
	"fmt"
	"time"
)

// RepositorySpec addresses one restic repository for one operation.
//
// It carries an absolute directory rather than a relative one because only the
// backup service, which owns the verified mount point, may turn a registered
// disk into a path. The adapter never constructs a repository location itself.
type RepositorySpec struct {
	// Directory is the absolute repository path below a verified mount point.
	Directory string
	// Credential addresses the repository password in the secret store. An
	// empty locator is refused: ROTUP does not operate unencrypted repositories.
	Credential CredentialHandle
}

// RepositoryInfo is the identity restic reports for a repository. It is read
// back from the repository itself, never assumed from the path it was found at.
type RepositoryInfo struct {
	// ID is restic's own stable repository identifier.
	ID string
	// Version is the repository format version restic reports.
	Version int
}

// BackupRequest is one snapshot invocation.
type BackupRequest struct {
	Repository RepositorySpec
	// Paths are the absolute directories to snapshot. They are the exact
	// read-set the backup service captured; the adapter adds nothing to them
	// and expands nothing.
	Paths []string
	// Tags are correlation labels attached to the snapshot. They must be safe
	// identifiers: the adapter refuses anything else rather than trusting the
	// caller.
	Tags []string
}

// BackupStats holds restic's own machine-readable counters for one snapshot.
// Available is false when restic produced no parsable summary, in which case
// nothing here may be trusted and no snapshot may be claimed.
type BackupStats struct {
	Available       bool
	SnapshotID      string
	FilesNew        int64
	FilesChanged    int64
	FilesUnmodified int64
	FilesProcessed  int64
	BytesProcessed  int64
	BytesAdded      int64
	// Errors counts the error messages restic emitted on its structured
	// output. A snapshot that restic itself reported errors for is not a
	// complete backup of the requested paths.
	Errors int64
}

// FailureSignal returns restic's own reason the run cannot be treated as a
// complete backup, or "" when everything it reported agrees it finished.
//
// This mirrors the rule the collection engine already follows: an exit status
// is not proof. A tool that exits zero while its structured output reports
// errors, or while it never named a snapshot, has told ROTUP the result is not
// what was asked for, and ROTUP must believe the output rather than the exit
// code. Reporting such a run as a successful backup would record a snapshot the
// engine itself does not stand behind.
func (s BackupStats) FailureSignal() string {
	switch {
	case !s.Available:
		return "restic produced no machine-readable summary, so the snapshot could not be confirmed"
	case s.SnapshotID == "":
		return "restic finished without naming a snapshot"
	case s.Errors > 0:
		return fmt.Sprintf("restic reported %d error(s) while reading the backup paths", s.Errors)
	default:
		return ""
	}
}

// BackupResult is returned from Backup even when the backup failed, so the
// caller can decide what the repository state must become.
type BackupResult struct {
	Stats BackupStats
	// Diagnostics is a bounded, redacted tail of restic output.
	Diagnostics string
	Duration    time.Duration
	Warnings    []string
}

// Snapshot is one snapshot as the repository reports it.
type Snapshot struct {
	ID       string
	ShortID  string
	Time     time.Time
	Tags     []string
	Paths    []string
	Hostname string
}

// HasTag reports whether the snapshot carries a tag.
func (s Snapshot) HasTag(tag string) bool {
	for _, candidate := range s.Tags {
		if candidate == tag {
			return true
		}
	}
	return false
}

// CheckResult is the outcome of a non-destructive repository check.
type CheckResult struct {
	// Errors is the number of problems the check reported. A repository is only
	// healthy when this is zero.
	Errors int64
	// Diagnostics is a bounded, redacted tail of the check output.
	Diagnostics string
	Duration    time.Duration
}

// BackupEngine is the seam over the external snapshot tool.
//
// Every method is either read-only or additive. There is deliberately no
// forget, prune, or unlock method: destructive repository operations belong to
// the retention milestone, and an interface that cannot express them cannot
// accidentally be made to perform one.
type BackupEngine interface {
	// Probe reports the discovered engine, or a typed error when it is
	// unusable. It touches no repository.
	Probe(ctx context.Context) (EngineInfo, error)
	// InitializeRepository creates a new restic repository. It fails if the
	// directory already holds one.
	InitializeRepository(ctx context.Context, spec RepositorySpec) (RepositoryInfo, error)
	// InspectRepository opens an existing repository and reads its identity
	// back. It is the proof that a repository is present, is structurally a
	// restic repository, and can be opened with the stored password.
	InspectRepository(ctx context.Context, spec RepositorySpec) (RepositoryInfo, error)
	// Backup creates one snapshot of the requested paths.
	Backup(ctx context.Context, request BackupRequest) (BackupResult, error)
	// Snapshots lists the repository's snapshots. It is the independent read
	// that confirms a snapshot the backup command claimed really exists.
	Snapshots(ctx context.Context, spec RepositorySpec) ([]Snapshot, error)
	// Check runs restic's non-destructive structural verification.
	Check(ctx context.Context, spec RepositorySpec) (CheckResult, error)
}
