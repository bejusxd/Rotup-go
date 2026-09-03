package staging

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// Phase records how far a collection run got. It is written into the run's work
// area so that the filesystem, not only SQLite, records whether the current
// tree may have been mutated.
type Phase string

const (
	// PhasePreflight means nothing has touched the current tree yet.
	PhasePreflight Phase = "PREFLIGHT"
	// PhaseMutating means the transfer engine was, or may still be, writing
	// into the current tree.
	PhaseMutating Phase = "MUTATING"
	// PhasePublished means the run completed and current is trustworthy.
	PhasePublished Phase = "PUBLISHED"
	// PhaseFailedBeforeMutation means the run failed while current was still
	// untouched, so the previous published state is intact.
	PhaseFailedBeforeMutation Phase = "FAILED_BEFORE_MUTATION"
	// PhaseFailedAfterMutation means current may hold a partially updated tree.
	PhaseFailedAfterMutation Phase = "FAILED_AFTER_MUTATION"
)

// MutationPossible reports whether the phase leaves the current tree in a state
// ROTUP cannot vouch for without another successful collection.
func (p Phase) MutationPossible() bool {
	switch p {
	case PhaseMutating, PhaseFailedAfterMutation:
		return true
	default:
		return false
	}
}

const (
	markerFileName   = "run.json"
	metadataFileName = "current.json"
)

// RunMarker is the durable per-run record inside the work area.
type RunMarker struct {
	Version    int    `json:"version"`
	RunID      string `json:"run_id"`
	SourceID   string `json:"source_id"`
	Policy     string `json:"reconciliation_policy"`
	Phase      Phase  `json:"phase"`
	CurrentDir string `json:"current_dir"`
	ArchiveDir string `json:"archive_dir,omitempty"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	Note       string `json:"note,omitempty"`
}

// WriteMarker atomically records the run's phase in its work area.
func WriteMarker(workDir string, marker RunMarker) error {
	marker.Version = 1
	marker.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return domain.WrapError(domain.ErrClassInternal, "staging_marker_failed",
			"the run marker could not be encoded", err)
	}
	if err := writeFileAtomic(filepath.Join(workDir, markerFileName), encoded); err != nil {
		return domain.WrapError(domain.ErrClassInternal, "staging_marker_failed",
			"the run marker could not be written", err)
	}
	return nil
}

// ReadMarker loads a run marker. A missing marker returns ok=false.
func ReadMarker(workDir string) (RunMarker, bool, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, markerFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RunMarker{}, false, nil
		}
		return RunMarker{}, false, domain.WrapError(domain.ErrClassInternal, "staging_marker_failed",
			"the run marker could not be read", err)
	}
	var marker RunMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		// A corrupt marker must never be read as "nothing happened".
		return RunMarker{}, false, domain.WrapError(domain.ErrClassIntegrity, "staging_marker_corrupt",
			"the run marker could not be decoded", err)
	}
	return marker, true, nil
}

// PublishedState is the small durable metadata written next to the source when
// a collection publishes a new stable current tree. It is audit and recovery
// metadata, not a historical filesystem version.
type PublishedState struct {
	Version      int    `json:"version"`
	SourceID     string `json:"source_id"`
	RunID        string `json:"run_id"`
	StateVersion int64  `json:"staging_state_version"`
	PublishedAt  string `json:"published_at"`
	Files        int64  `json:"files"`
	Bytes        int64  `json:"bytes"`
}

// WritePublishedState atomically records the published current-state metadata.
func WritePublishedState(metadataDir string, state PublishedState) error {
	state.Version = 1
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return domain.WrapError(domain.ErrClassInternal, "staging_metadata_failed",
			"the published state metadata could not be encoded", err)
	}
	if err := writeFileAtomic(filepath.Join(metadataDir, metadataFileName), encoded); err != nil {
		return domain.WrapError(domain.ErrClassInternal, "staging_metadata_failed",
			"the published state metadata could not be written", err)
	}
	return nil
}

// ReadPublishedState loads the published current-state metadata if present.
func ReadPublishedState(metadataDir string) (PublishedState, bool, error) {
	raw, err := os.ReadFile(filepath.Join(metadataDir, metadataFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishedState{}, false, nil
		}
		return PublishedState{}, false, domain.WrapError(domain.ErrClassInternal, "staging_metadata_failed",
			"the published state metadata could not be read", err)
	}
	var state PublishedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return PublishedState{}, false, domain.WrapError(domain.ErrClassIntegrity, "staging_metadata_corrupt",
			"the published state metadata could not be decoded", err)
	}
	return state, true, nil
}

// StaleWork describes a work area whose run is no longer active.
type StaleWork struct {
	SourceID  string
	RunID     string
	Path      string
	Marker    RunMarker
	HasMarker bool
	// MarkerUnreadable is set when a marker exists but could not be decoded.
	// Such a work area must be treated as if mutation had happened.
	MarkerUnreadable bool
}

// MutationPossible reports whether this work area may have left the current
// tree partially updated.
//
// An unreadable marker counts as possible, because a state ROTUP cannot read is
// a state it cannot vouch for. A work area with no marker at all is the one
// provably safe case: preflight creates the work directory and immediately
// writes the PREFLIGHT marker, and no engine is started in between, so a work
// directory without a marker cannot have reached the transfer.
func (w StaleWork) MutationPossible() bool {
	if w.MarkerUnreadable {
		return true
	}
	if !w.HasMarker {
		return false
	}
	return w.Marker.Phase.MutationPossible()
}

// ScanWork lists every work area belonging to a source. It is used by startup
// recovery to decide whether an interrupted run could have mutated current.
func (l *Layout) ScanWork(sourceID string) ([]StaleWork, error) {
	dir, err := l.SourceWorkDir(sourceID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, domain.WrapError(domain.ErrClassInternal, "staging_read_failed",
			"the source work directory could not be listed", err)
	}

	result := make([]StaleWork, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !domain.IsID(entry.Name()) {
			continue
		}
		workDir, pathErr := l.WorkDir(sourceID, entry.Name())
		if pathErr != nil {
			continue
		}
		stale := StaleWork{SourceID: sourceID, RunID: entry.Name(), Path: workDir}
		marker, ok, markerErr := ReadMarker(workDir)
		switch {
		case markerErr != nil:
			stale.MarkerUnreadable = true
		case ok:
			stale.HasMarker = true
			stale.Marker = marker
		}
		result = append(result, stale)
	}
	return result, nil
}

// writeFileAtomic writes contents through a temporary file in the same
// directory, flushes it, and renames it into place.
func writeFileAtomic(path string, contents []byte) error {
	dir := filepath.Dir(path)
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	// Windows cannot rename onto an existing file.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if handle, openErr := os.Open(dir); openErr == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
