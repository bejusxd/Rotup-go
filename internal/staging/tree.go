package staging

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// MaxWalkEntries bounds every staging walk so a pathological tree cannot make
// a preflight check or a UI request run unbounded.
const MaxWalkEntries = 2_000_000

// TreeStats summarizes a staging subtree.
type TreeStats struct {
	Files     int64
	Bytes     int64
	Truncated bool
}

// Empty reports whether the subtree holds no regular files.
func (s TreeStats) Empty() bool { return s.Files == 0 }

// Measure walks a staging subtree counting regular files and bytes. Symbolic
// links are counted as entries but never followed, so a link planted inside
// staging cannot redirect the walk outside the root. A missing directory is
// reported as an empty tree, not an error.
func Measure(dir string) (TreeStats, error) {
	var stats TreeStats
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && path == dir {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if stats.Files >= MaxWalkEntries {
			stats.Truncated = true
			return filepath.SkipAll
		}
		stats.Files++
		// Type() comes from the directory entry and reports a symlink without
		// following it. Info() on a symlink entry therefore describes the link.
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		if info.Mode().IsRegular() {
			stats.Bytes += info.Size()
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TreeStats{}, domain.WrapError(domain.ErrClassInternal, "staging_walk_failed",
			"a staging directory could not be inspected", err)
	}
	return stats, nil
}

// Entry is one row of a read-only staging listing shown in the UI.
type Entry struct {
	Name       string
	IsDir      bool
	IsSymlink  bool
	IsRegular  bool
	Size       int64
	ModifiedAt time.Time
}

// Listing is a bounded, read-only view of one staging directory.
type Listing struct {
	// RelativePath is the slash-separated path below the listed root.
	RelativePath string
	Parent       string
	HasParent    bool
	Entries      []Entry
	Truncated    bool
	Missing      bool
}

// MaxListingEntries bounds one UI directory listing.
const MaxListingEntries = 500

// List returns a bounded listing of relative below base. base must already be a
// path this layout produced. Symbolic links are reported but never followed,
// and relative is validated so it cannot escape base.
func (l *Layout) List(base, relative string) (Listing, error) {
	normalized, err := domain.NormalizeRelativePath(relative, "path")
	if err != nil {
		return Listing{}, err
	}
	target := base
	if normalized != "" {
		target = filepath.Join(base, filepath.FromSlash(normalized))
	}
	if !withinPath(base, filepath.Clean(target)) {
		return Listing{}, domain.NewError(domain.ErrClassInvariant, "staging_path_escape",
			"the requested path is outside the source's staging tree")
	}
	if err := l.AssertWithinRoot(target); err != nil {
		return Listing{}, err
	}

	listing := Listing{RelativePath: normalized}
	if normalized != "" {
		listing.HasParent = true
		if index := strings.LastIndex(normalized, "/"); index >= 0 {
			listing.Parent = normalized[:index]
		}
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			listing.Missing = true
			return listing, nil
		}
		return Listing{}, domain.WrapError(domain.ErrClassInternal, "staging_read_failed",
			"the staging directory could not be listed", err)
	}
	if len(entries) > MaxListingEntries {
		entries = entries[:MaxListingEntries]
		listing.Truncated = true
	}
	for _, entry := range entries {
		row := Entry{Name: entry.Name(), IsDir: entry.IsDir()}
		// Info on a DirEntry from ReadDir describes the entry itself; a symlink
		// is not resolved, so a link to outside staging is never stat'ed.
		if info, infoErr := entry.Info(); infoErr == nil {
			row.IsSymlink = info.Mode()&os.ModeSymlink != 0
			row.IsRegular = info.Mode().IsRegular()
			row.Size = info.Size()
			row.ModifiedAt = info.ModTime().UTC()
		}
		listing.Entries = append(listing.Entries, row)
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		if listing.Entries[i].IsDir != listing.Entries[j].IsDir {
			return listing.Entries[i].IsDir
		}
		return listing.Entries[i].Name < listing.Entries[j].Name
	})
	return listing, nil
}
