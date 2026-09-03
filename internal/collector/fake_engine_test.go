package collector

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// fakeEngine stands in for rclone in collector tests. It performs real
// filesystem work on real temporary directories so that the staging outcome
// being asserted is the outcome an engine would actually produce.
//
// Its transfer semantics deliberately mirror the documented contract the rclone
// adapter pins:
//
//   - TransferCopy behaves like "rclone copy": additions and updates only,
//     nothing is ever removed from the destination.
//   - TransferSyncWithArchive behaves like "rclone sync --backup-dir
//     --delete-after": every destination object that is replaced or that no
//     longer exists at the source is moved into the archive directory rather
//     than deleted, and removals happen only after transfers succeed.
//
// Whether a real rclone binary honours that contract is what the opt-in spike
// in the rclone adapter package verifies; this fake proves that ROTUP's own
// staging, state, and history behaviour is correct given that contract.
type fakeEngine struct {
	mu sync.Mutex

	// sourceDir is a real local directory standing in for the SMB share.
	sourceDir string

	// connectErr, measureErr and collectErr force the corresponding call to
	// fail, which is how the unavailable-source scenarios are driven.
	connectErr error
	measureErr error
	collectErr error

	// statsUnavailable simulates an engine that produced no machine-readable
	// statistics, so ROTUP cannot prove whether it mutated the destination.
	statsUnavailable bool
	// mutateBeforeFailing performs the transfer and then reports failure, which
	// is what an interrupted or partially failed run looks like.
	mutateBeforeFailing bool
	// engineErrors, fatalError, retryError and extraQueuedTransfers model the
	// counters a real engine can report while still exiting cleanly. Each is a
	// statement that the destination is not a complete transfer.
	engineErrors         int64
	fatalError           bool
	retryError           bool
	extraQueuedTransfers int64

	// warnings are advisory engine notes that do not imply incompleteness.
	warnings []string

	collectCalls int
}

func (f *fakeEngine) Probe(context.Context) (ports.EngineInfo, error) {
	return ports.EngineInfo{Name: "fake", Version: "0.0.0"}, nil
}

func (f *fakeEngine) TestConnection(context.Context, ports.SourceSpec, ports.CredentialHandle) (ports.ConnectionResult, error) {
	if f.connectErr != nil {
		return ports.ConnectionResult{}, f.connectErr
	}
	entries, err := os.ReadDir(f.sourceDir)
	if err != nil {
		return ports.ConnectionResult{}, domain.WrapError(domain.ErrClassUnavailable, "fake_unreachable",
			"the fake source could not be listed", err)
	}
	return ports.ConnectionResult{Entries: len(entries)}, nil
}

func (f *fakeEngine) Measure(context.Context, ports.SourceSpec, ports.CredentialHandle) (ports.SourceSize, error) {
	if f.measureErr != nil {
		return ports.SourceSize{}, f.measureErr
	}
	files, bytes, err := walkTree(f.sourceDir)
	if err != nil {
		return ports.SourceSize{}, err
	}
	return ports.SourceSize{Count: files, Bytes: bytes}, nil
}

func (f *fakeEngine) Collect(_ context.Context, request ports.TransferRequest) (ports.TransferResult, error) {
	f.mu.Lock()
	f.collectCalls++
	f.mu.Unlock()

	if f.collectErr != nil && !f.mutateBeforeFailing {
		// A failure before any write: the engine reports statistics proving it
		// changed nothing.
		result := ports.TransferResult{Stats: ports.TransferStats{Available: !f.statsUnavailable}}
		return result, f.collectErr
	}

	stats, err := f.transfer(request)
	if err != nil {
		return ports.TransferResult{}, err
	}
	stats.Available = !f.statsUnavailable
	stats.Errors = f.engineErrors
	stats.FatalError = f.fatalError
	stats.RetryError = f.retryError
	stats.TotalTransfers += f.extraQueuedTransfers

	result := ports.TransferResult{Stats: stats, Warnings: f.warnings}
	if f.collectErr != nil {
		return result, f.collectErr
	}
	return result, nil
}

// transfer applies the requested mode to the real filesystem and returns the
// counters an engine would report.
func (f *fakeEngine) transfer(request ports.TransferRequest) (ports.TransferStats, error) {
	sourceFiles, err := relativeFiles(f.sourceDir)
	if err != nil {
		return ports.TransferStats{}, err
	}
	destinationFiles, err := relativeFiles(request.DestinationDir)
	if err != nil {
		return ports.TransferStats{}, err
	}

	var stats ports.TransferStats
	archiving := request.Mode == ports.TransferSyncWithArchive

	for relative, sourceContent := range sourceFiles {
		destinationPath := filepath.Join(request.DestinationDir, filepath.FromSlash(relative))
		existing, exists := destinationFiles[relative]
		if exists && existing == sourceContent {
			stats.Checks++
			continue
		}
		if exists && archiving {
			if err := archiveFile(request.ArchiveDir, relative, existing); err != nil {
				return stats, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
			return stats, err
		}
		if err := os.WriteFile(destinationPath, []byte(sourceContent), 0o600); err != nil {
			return stats, err
		}
		stats.Transfers++
		stats.Bytes += int64(len(sourceContent))
	}
	stats.TotalTransfers = stats.Transfers
	stats.TotalBytes = stats.Bytes

	if !archiving {
		return stats, nil
	}
	// Removals happen only after every transfer has succeeded, which is what
	// --delete-after guarantees.
	for relative, content := range destinationFiles {
		if _, stillAtSource := sourceFiles[relative]; stillAtSource {
			continue
		}
		if err := archiveFile(request.ArchiveDir, relative, content); err != nil {
			return stats, err
		}
		if err := os.Remove(filepath.Join(request.DestinationDir, filepath.FromSlash(relative))); err != nil {
			return stats, err
		}
		stats.Deletes++
	}
	return stats, nil
}

func archiveFile(archiveDir, relative, content string) error {
	if archiveDir == "" {
		return domain.NewError(domain.ErrClassInvariant, "fake_no_archive_dir",
			"an archiving transfer was requested without an archive directory")
	}
	target := filepath.Join(archiveDir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(content), 0o600)
}

// relativeFiles reads a tree into a map of slash-relative path to contents.
func relativeFiles(root string) (map[string]string, error) {
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return result, nil
}

func walkTree(root string) (int64, int64, error) {
	files, err := relativeFiles(root)
	if err != nil {
		return 0, 0, err
	}
	var count, bytes int64
	for _, content := range files {
		count++
		bytes += int64(len(content))
	}
	return count, bytes, nil
}
