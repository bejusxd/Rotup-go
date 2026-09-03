// Package staging owns the staging filesystem layout. It is the only package
// allowed to turn identifiers and administrator-supplied relative paths into
// absolute staging paths, and every path it produces is proven to stay below
// the configured staging root.
package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
)

// Directory names of the architecture staging layout.
const (
	SourcesDirName   = "sources"
	RevisionsDirName = "revisions"
	WorkDirName      = "work"
	MetadataDirName  = "metadata"
	LostFoundDirName = "lost+found"
	CurrentDirName   = "current"

	// DirMode is the mode requested for every ROTUP-owned staging directory.
	DirMode os.FileMode = 0o700
)

// Layout resolves staging paths below a single root.
type Layout struct {
	root string
	// realRoot is the symlink-resolved root, used for containment proofs.
	realRoot string
}

// NewLayout resolves the staging root and creates the top-level layout.
func NewLayout(root string) (*Layout, error) {
	if strings.TrimSpace(root) == "" {
		return nil, domain.NewError(domain.ErrClassConfiguration, "staging_root_empty",
			"staging root must be configured")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "staging_root_invalid",
			"staging root could not be resolved", err)
	}
	if err := os.MkdirAll(absolute, DirMode); err != nil {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "staging_root_unusable",
			"staging root could not be created", err)
	}
	realRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "staging_root_unusable",
			"staging root could not be resolved through symlinks", err)
	}

	layout := &Layout{root: absolute, realRoot: realRoot}
	for _, name := range []string{SourcesDirName, RevisionsDirName, WorkDirName, MetadataDirName, LostFoundDirName} {
		if err := os.MkdirAll(filepath.Join(absolute, name), DirMode); err != nil {
			return nil, domain.WrapError(domain.ErrClassConfiguration, "staging_root_unusable",
				fmt.Sprintf("staging directory %q could not be created", name), err)
		}
	}
	return layout, nil
}

// Root returns the configured staging root.
func (l *Layout) Root() string { return l.root }

// SourcePaths are the absolute paths one collection run works with.
type SourcePaths struct {
	// Current is the source's reconciled published tree.
	Current string
	// Work is the run's disposable scratch area.
	Work string
	// Archive receives displaced and deleted objects for this run. It is empty
	// for policies that never archive.
	Archive string
	// Metadata holds the source's small durable staging metadata.
	Metadata string
}

// CurrentDir returns the source's current tree.
func (l *Layout) CurrentDir(source domain.Source) (string, error) {
	relative, err := domain.NormalizeRelativePath(source.StagingRelativePath, "staging destination")
	if err != nil {
		return "", err
	}
	if relative == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "staging_path_empty",
			"source has no staging destination")
	}
	return l.resolve(SourcesDirName, filepath.FromSlash(relative), CurrentDirName)
}

// WorkDir returns the per-run work area for a source.
func (l *Layout) WorkDir(sourceID, runID string) (string, error) {
	if err := requireIDs(sourceID, runID); err != nil {
		return "", err
	}
	return l.resolve(WorkDirName, sourceID, runID)
}

// RevisionDir returns the per-run revision batch directory for a source.
func (l *Layout) RevisionDir(sourceID, runID string) (string, error) {
	if err := requireIDs(sourceID, runID); err != nil {
		return "", err
	}
	return l.resolve(RevisionsDirName, sourceID, runID)
}

// SourceRevisionsDir returns the directory holding every revision batch of a
// source.
func (l *Layout) SourceRevisionsDir(sourceID string) (string, error) {
	if err := requireIDs(sourceID); err != nil {
		return "", err
	}
	return l.resolve(RevisionsDirName, sourceID)
}

// SourceWorkDir returns the directory holding every work area of a source.
func (l *Layout) SourceWorkDir(sourceID string) (string, error) {
	if err := requireIDs(sourceID); err != nil {
		return "", err
	}
	return l.resolve(WorkDirName, sourceID)
}

// MetadataDir returns the source's staging metadata directory.
func (l *Layout) MetadataDir(sourceID string) (string, error) {
	if err := requireIDs(sourceID); err != nil {
		return "", err
	}
	return l.resolve(MetadataDirName, sourceID)
}

// RevisionRelativePath returns the root-relative slash path of a revision batch,
// which is what is recorded in SQLite.
func RevisionRelativePath(sourceID, runID string) string {
	return RevisionsDirName + "/" + sourceID + "/" + runID
}

// PreparePaths creates and returns every directory one collection run needs.
// archive is only created when the policy archives displaced objects.
func (l *Layout) PreparePaths(source domain.Source, runID string, archive bool) (SourcePaths, error) {
	current, err := l.CurrentDir(source)
	if err != nil {
		return SourcePaths{}, err
	}
	work, err := l.WorkDir(source.ID, runID)
	if err != nil {
		return SourcePaths{}, err
	}
	metadata, err := l.MetadataDir(source.ID)
	if err != nil {
		return SourcePaths{}, err
	}
	paths := SourcePaths{Current: current, Work: work, Metadata: metadata}
	if archive {
		archiveDir, archiveErr := l.RevisionDir(source.ID, runID)
		if archiveErr != nil {
			return SourcePaths{}, archiveErr
		}
		paths.Archive = archiveDir
	}

	// The current tree and the work area are created up front. The archive
	// directory is deliberately not created here: an empty archive directory
	// must not exist for a run that displaced nothing, because its presence is
	// what tells recovery that a batch may exist.
	for _, dir := range []string{current, work, metadata} {
		if err := os.MkdirAll(dir, DirMode); err != nil {
			return SourcePaths{}, domain.WrapError(domain.ErrClassCapacity, "staging_mkdir_failed",
				"a staging directory could not be created", err)
		}
	}
	if err := l.AssertWithinRoot(current); err != nil {
		return SourcePaths{}, err
	}
	return paths, nil
}

// resolve joins parts below the root and proves the result stays inside it.
func (l *Layout) resolve(parts ...string) (string, error) {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." ||
			strings.Contains(part, "..") && containsTraversalSegment(part) {
			return "", domain.NewError(domain.ErrClassInvariant, "staging_path_invalid",
				"a staging path component is not safe")
		}
	}
	joined := filepath.Join(append([]string{l.root}, parts...)...)
	cleaned := filepath.Clean(joined)
	if !withinPath(l.root, cleaned) {
		return "", domain.NewError(domain.ErrClassInvariant, "staging_path_escape",
			"a staging path escaped the staging root")
	}
	return cleaned, nil
}

// AssertWithinRoot proves that a path, after symlink resolution, is still below
// the staging root. Non-existent leaves are checked through their nearest
// existing ancestor so the guard also covers paths about to be created.
func (l *Layout) AssertWithinRoot(path string) error {
	if !withinPath(l.root, filepath.Clean(path)) {
		return domain.NewError(domain.ErrClassInvariant, "staging_path_escape",
			"a staging path escaped the staging root")
	}
	candidate := filepath.Clean(path)
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			if !withinPath(l.realRoot, resolved) {
				return domain.NewError(domain.ErrClassInvariant, "staging_symlink_escape",
					"a staging path resolves outside the staging root through a symbolic link")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return domain.WrapError(domain.ErrClassInternal, "staging_path_unresolvable",
				"a staging path could not be resolved", err)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return domain.NewError(domain.ErrClassInvariant, "staging_path_unresolvable",
				"a staging path could not be resolved to the staging root")
		}
		candidate = parent
	}
}

func withinPath(root, candidate string) bool {
	if candidate == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(candidate, prefix)
}

func containsTraversalSegment(part string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(part), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func requireIDs(values ...string) error {
	for _, value := range values {
		if !domain.IsID(value) {
			return domain.NewError(domain.ErrClassInvariant, "staging_id_invalid",
				"a staging path component is not an identifier ROTUP generated")
		}
	}
	return nil
}
