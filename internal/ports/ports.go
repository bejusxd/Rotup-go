// Package ports declares the narrow interfaces that adapters implement. It
// depends only on the domain package so that core packages never need to import
// SQLite, HTTP, or an external-tool implementation.
package ports

import (
	"context"
	"fmt"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// CommandSpec fully describes one subprocess invocation. There is deliberately
// no command-string field: callers supply an executable path and an argument
// array so nothing is ever handed to a shell.
type CommandSpec struct {
	// Path is the executable to run. It is never resolved from an inherited
	// PATH at this layer.
	Path string
	// Args are the arguments after argv[0].
	Args []string
	// Env completely replaces the child environment. A nil slice means "empty
	// environment", not "inherit".
	Env []string
	// Dir is the working directory, or "" for the parent's.
	Dir string
	// StdoutTailLimit and StderrTailLimit bound how many trailing bytes of each
	// stream are retained. Zero selects the runner's default.
	StdoutTailLimit int
	StderrTailLimit int
}

// ProcessResult reports how a subprocess finished. Stdout and Stderr hold only
// the retained tail of each stream.
type ProcessResult struct {
	Started         bool
	Exited          bool
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Canceled        bool
	Duration        time.Duration
	// Err is set for failures to start, wait, or capture output. A non-zero
	// exit code alone is not an error here; it is reported through ExitCode.
	Err error
}

// ProcessRunner starts an external tool and returns its bounded result. M2 only
// needs the synchronous, fully buffered form used by the rclone adapter.
type ProcessRunner interface {
	Run(ctx context.Context, spec CommandSpec) ProcessResult
}

// SourceSpec is the engine-neutral description of what to read. It carries
// semantic SMB fields rather than a prebuilt UNC path or engine argument text.
type SourceSpec struct {
	Host         string
	Share        string
	Subdirectory string
	Username     string
	IncludeRules []string
	ExcludeRules []string
}

// CredentialHandle addresses stored secret material without carrying it. An
// empty Locator means the source has no stored password.
type CredentialHandle struct {
	Locator string
}

// EngineInfo identifies a discovered external tool.
type EngineInfo struct {
	Name    string
	Version string
	Path    string
}

// ConnectionResult is the outcome of a read-only reachability probe.
type ConnectionResult struct {
	// Entries is how many top-level entries the probe listed. It is capped and
	// is only an indication that the path is readable.
	Entries int
	// Duration is how long the probe took.
	Duration time.Duration
}

// SourceSize is the preflight enumeration of a source, used to detect a source
// that has unexpectedly become empty before anything mutates staging.
type SourceSize struct {
	Count int64
	Bytes int64
}

// TransferMode selects the reconciliation shape the engine must execute. It is
// derived from the source's reconciliation policy by the collector, never by
// the adapter.
type TransferMode string

const (
	// TransferCopy adds and updates destination objects and removes nothing.
	TransferCopy TransferMode = "COPY"
	// TransferSyncWithArchive makes the destination match the source, moving
	// every displaced or removed object into the archive directory first.
	TransferSyncWithArchive TransferMode = "SYNC_WITH_ARCHIVE"
)

// TransferRequest is one collection invocation. Every filesystem path in it is
// absolute and was constructed by the staging package.
type TransferRequest struct {
	Source     SourceSpec
	Credential CredentialHandle
	Mode       TransferMode
	// DestinationDir is the source's current tree.
	DestinationDir string
	// ArchiveDir receives displaced and deleted objects. It must be empty for
	// TransferCopy and non-empty for TransferSyncWithArchive.
	ArchiveDir string
	// TempDir is the run's work area, used for engine temporary files.
	TempDir string
}

// TransferStats holds the engine's own machine-readable counters. Available is
// false when the engine produced no parsable statistics, in which case no
// counter may be trusted.
type TransferStats struct {
	Available      bool
	Bytes          int64
	TotalBytes     int64
	Checks         int64
	Transfers      int64
	TotalTransfers int64
	Deletes        int64
	Renames        int64
	Errors         int64
	FatalError     bool
	RetryError     bool
}

// MutationObserved reports whether the engine says it changed the destination.
// It is only meaningful when Available is true.
func (s TransferStats) MutationObserved() bool {
	return s.Transfers > 0 || s.Deletes > 0 || s.Renames > 0
}

// FailureSignal returns the engine's own reason the run cannot be treated as a
// complete transfer, or "" when every counter agrees it finished cleanly. It is
// only meaningful when Available is true.
//
// This exists because an exit status is not proof on its own. An engine that
// exits zero while reporting a fatal error, exhausted retries, per-object
// errors, or fewer completed transfers than it queued has told ROTUP the
// destination is incomplete, and ROTUP must believe the counters rather than
// the exit code. Publishing such a tree as stable would hand a later backup a
// view the engine itself does not stand behind.
func (s TransferStats) FailureSignal() string {
	switch {
	case s.FatalError:
		return "the engine reported a fatal error"
	case s.RetryError:
		return "the engine exhausted its retries without completing"
	case s.Errors > 0:
		return fmt.Sprintf("the engine reported %d error(s) while transferring", s.Errors)
	case s.Transfers < s.TotalTransfers:
		return fmt.Sprintf("only %d of the %d queued transfers completed", s.Transfers, s.TotalTransfers)
	default:
		return ""
	}
}

// TransferResult is returned from Collect even when the transfer failed, so the
// collector can decide whether staging may still be trusted.
type TransferResult struct {
	Stats TransferStats
	// Diagnostics is a bounded, redacted tail of engine output.
	Diagnostics string
	Duration    time.Duration
	// Warnings are safe, bounded, administrator-facing notes.
	Warnings []string
}

// TransferEngine is the seam over the external collection tool.
type TransferEngine interface {
	// Probe reports the discovered engine, or a typed error when it is
	// unusable. It performs no source access.
	Probe(ctx context.Context) (EngineInfo, error)
	// TestConnection performs a read-only reachability and readability check.
	// It must never mutate staging.
	TestConnection(ctx context.Context, spec SourceSpec, credential CredentialHandle) (ConnectionResult, error)
	// Measure enumerates the source without writing anything.
	Measure(ctx context.Context, spec SourceSpec, credential CredentialHandle) (SourceSize, error)
	// Collect performs the transfer described by the request.
	Collect(ctx context.Context, request TransferRequest) (TransferResult, error)
}

// SecretStore holds plaintext secret material outside SQLite and scopes access
// to a callback.
type SecretStore interface {
	// Put stores new material and returns its opaque provider locator.
	Put(ctx context.Context, purpose domain.SecretPurpose, value string) (string, error)
	// Materialize invokes fn with the stored material. The material must not be
	// retained after fn returns.
	Materialize(ctx context.Context, locator string, fn func(domain.SecretMaterial) error) error
	// Delete permanently removes stored material. Deleting a locator that is
	// already gone is not an error.
	Delete(ctx context.Context, locator string) error
	// Provider names the storage provider for credential_refs metadata.
	Provider() string
}
