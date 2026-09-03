package staging

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

func newTestLayout(t *testing.T) *Layout {
	t.Helper()
	layout, err := NewLayout(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatalf("new layout: %v", err)
	}
	return layout
}

func testSource() domain.Source {
	return domain.Source{
		ID:                  "11111111-1111-4111-8111-111111111111",
		StagingRelativePath: "team/documents",
	}
}

func TestLayoutCreatesTheArchitectureDirectories(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	for _, name := range []string{SourcesDirName, RevisionsDirName, WorkDirName, MetadataDirName, LostFoundDirName} {
		info, err := os.Stat(filepath.Join(layout.Root(), name))
		if err != nil {
			t.Fatalf("staging directory %q was not created: %v", name, err)
		}
		if !info.IsDir() {
			t.Fatalf("staging entry %q is not a directory", name)
		}
	}
}

func TestLayoutBuildsPathsBelowTheRoot(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()
	runID := "22222222-2222-4222-8222-222222222222"

	current, err := layout.CurrentDir(source)
	if err != nil {
		t.Fatalf("current dir: %v", err)
	}
	want := filepath.Join(layout.Root(), SourcesDirName, "team", "documents", CurrentDirName)
	if current != want {
		t.Fatalf("current dir = %q, want %q", current, want)
	}

	work, err := layout.WorkDir(source.ID, runID)
	if err != nil {
		t.Fatalf("work dir: %v", err)
	}
	if work != filepath.Join(layout.Root(), WorkDirName, source.ID, runID) {
		t.Fatalf("work dir = %q", work)
	}

	revision, err := layout.RevisionDir(source.ID, runID)
	if err != nil {
		t.Fatalf("revision dir: %v", err)
	}
	if revision != filepath.Join(layout.Root(), RevisionsDirName, source.ID, runID) {
		t.Fatalf("revision dir = %q", revision)
	}
	if got := RevisionRelativePath(source.ID, runID); got != "revisions/"+source.ID+"/"+runID {
		t.Fatalf("revision relative path = %q", got)
	}
}

func TestLayoutRejectsUnsafePathComponents(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)

	source := testSource()
	source.StagingRelativePath = "../escape"
	if _, err := layout.CurrentDir(source); err == nil {
		t.Fatal("a traversing staging destination must be rejected")
	}
	source.StagingRelativePath = "/absolute"
	if _, err := layout.CurrentDir(source); err == nil {
		t.Fatal("an absolute staging destination must be rejected")
	}
	source.StagingRelativePath = ""
	if _, err := layout.CurrentDir(source); err == nil {
		t.Fatal("an empty staging destination must be rejected")
	}

	// Identifiers that did not come from ROTUP can never become path segments.
	for _, id := range []string{"..", "../escape", "not-an-id", ""} {
		if _, err := layout.WorkDir(id, "22222222-2222-4222-8222-222222222222"); err == nil {
			t.Fatalf("work dir accepted an invalid source identifier %q", id)
		}
		if _, err := layout.RevisionDir("11111111-1111-4111-8111-111111111111", id); err == nil {
			t.Fatalf("revision dir accepted an invalid run identifier %q", id)
		}
	}
}

func TestAssertWithinRootRejectsASymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links on Windows requires elevation or developer mode")
	}
	t.Parallel()

	base := t.TempDir()
	layout, err := NewLayout(filepath.Join(base, "staging"))
	if err != nil {
		t.Fatalf("new layout: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create outside directory: %v", err)
	}
	link := filepath.Join(layout.Root(), SourcesDirName, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if err := layout.AssertWithinRoot(filepath.Join(link, CurrentDirName)); err == nil {
		t.Fatal("a path resolving outside the root through a symbolic link must be rejected")
	}
	if err := layout.AssertWithinRoot(filepath.Join(layout.Root(), SourcesDirName)); err != nil {
		t.Fatalf("a path inside the root was rejected: %v", err)
	}
}

func TestPreparePathsCreatesCurrentAndWorkButNotTheArchive(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()
	runID := "22222222-2222-4222-8222-222222222222"

	paths, err := layout.PreparePaths(source, runID, true)
	if err != nil {
		t.Fatalf("prepare paths: %v", err)
	}
	for _, dir := range []string{paths.Current, paths.Work, paths.Metadata} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("directory %q was not created: %v", dir, err)
		}
	}
	// An archive directory that exists but is empty would be indistinguishable
	// from a batch that displaced nothing, so it must not be created up front.
	if _, err := os.Stat(paths.Archive); !os.IsNotExist(err) {
		t.Fatalf("the archive directory must not be created before anything is displaced (err=%v)", err)
	}

	copyOnly, err := layout.PreparePaths(source, runID, false)
	if err != nil {
		t.Fatalf("prepare paths without archiving: %v", err)
	}
	if copyOnly.Archive != "" {
		t.Fatalf("a non-archiving policy must not be given an archive directory, got %q", copyOnly.Archive)
	}
}

func TestMeasureCountsFilesAndBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stats, err := Measure(dir)
	if err != nil {
		t.Fatalf("measure empty: %v", err)
	}
	if !stats.Empty() {
		t.Fatalf("an empty directory measured %#v", stats)
	}

	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	stats, err = Measure(dir)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if stats.Files != 1 || stats.Bytes != 5 {
		t.Fatalf("measured %#v, want 1 file of 5 bytes", stats)
	}

	// A missing directory is an empty tree, not an error: a source that has
	// never been collected has no current directory yet.
	stats, err = Measure(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatalf("measure missing: %v", err)
	}
	if !stats.Empty() {
		t.Fatalf("a missing directory measured %#v", stats)
	}
}

func TestListIsBoundedAndCannotEscapeItsBase(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()
	paths, err := layout.PreparePaths(source, "22222222-2222-4222-8222-222222222222", false)
	if err != nil {
		t.Fatalf("prepare paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Current, "reports"), 0o700); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Current, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	listing, err := layout.List(paths.Current, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("listing has %d entries, want 2", len(listing.Entries))
	}
	if !listing.Entries[0].IsDir {
		t.Fatal("directories must sort before files")
	}
	if listing.HasParent {
		t.Fatal("the base directory has no parent inside the listing")
	}

	nested, err := layout.List(paths.Current, "reports")
	if err != nil {
		t.Fatalf("list nested: %v", err)
	}
	if !nested.HasParent || nested.Parent != "" {
		t.Fatalf("nested listing parent = %q, hasParent = %v", nested.Parent, nested.HasParent)
	}

	for _, escape := range []string{"..", "../..", "/etc", `C:\`} {
		if _, err := layout.List(paths.Current, escape); err == nil {
			t.Fatalf("listing path %q should have been rejected", escape)
		}
	}

	missing, err := layout.List(paths.Current, "does-not-exist")
	if err != nil {
		t.Fatalf("list missing: %v", err)
	}
	if !missing.Missing {
		t.Fatal("a missing directory must be reported as missing rather than as an error")
	}
}

func TestMarkerProtocolRecordsPhases(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()
	runID := "22222222-2222-4222-8222-222222222222"
	paths, err := layout.PreparePaths(source, runID, true)
	if err != nil {
		t.Fatalf("prepare paths: %v", err)
	}

	if _, ok, err := ReadMarker(paths.Work); err != nil || ok {
		t.Fatalf("a fresh work area must have no marker (ok=%v err=%v)", ok, err)
	}

	marker := RunMarker{RunID: runID, SourceID: source.ID, Phase: PhasePreflight}
	if err := WriteMarker(paths.Work, marker); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	loaded, ok, err := ReadMarker(paths.Work)
	if err != nil || !ok {
		t.Fatalf("read marker: ok=%v err=%v", ok, err)
	}
	if loaded.Phase != PhasePreflight || loaded.RunID != runID {
		t.Fatalf("marker = %#v", loaded)
	}
	if loaded.UpdatedAt == "" {
		t.Fatal("a marker must record when it was written")
	}

	marker.Phase = PhaseMutating
	if err := WriteMarker(paths.Work, marker); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}
	loaded, _, err = ReadMarker(paths.Work)
	if err != nil {
		t.Fatalf("reread marker: %v", err)
	}
	if loaded.Phase != PhaseMutating {
		t.Fatalf("marker phase = %s, want MUTATING", loaded.Phase)
	}
}

func TestScanWorkClassifiesMutationRisk(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()

	write := func(runID string, phase Phase) {
		t.Helper()
		dir, err := layout.WorkDir(source.ID, runID)
		if err != nil {
			t.Fatalf("work dir: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create work dir: %v", err)
		}
		if phase != "" {
			if err := WriteMarker(dir, RunMarker{RunID: runID, SourceID: source.ID, Phase: phase}); err != nil {
				t.Fatalf("write marker: %v", err)
			}
		}
	}

	preflightRun := "22222222-2222-4222-8222-222222222222"
	mutatingRun := "33333333-3333-4333-8333-333333333333"
	publishedRun := "44444444-4444-4444-8444-444444444444"
	bareRun := "55555555-5555-4555-8555-555555555555"
	corruptRun := "66666666-6666-4666-8666-666666666666"

	write(preflightRun, PhasePreflight)
	write(mutatingRun, PhaseMutating)
	write(publishedRun, PhasePublished)
	write(bareRun, "")
	write(corruptRun, PhasePreflight)
	corruptDir, err := layout.WorkDir(source.ID, corruptRun)
	if err != nil {
		t.Fatalf("work dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "run.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt marker: %v", err)
	}

	areas, err := layout.ScanWork(source.ID)
	if err != nil {
		t.Fatalf("scan work: %v", err)
	}
	byRun := make(map[string]StaleWork, len(areas))
	for _, area := range areas {
		byRun[area.RunID] = area
	}
	if len(byRun) != 5 {
		t.Fatalf("scanned %d work areas, want 5", len(byRun))
	}

	expected := map[string]bool{
		preflightRun: false,
		mutatingRun:  true,
		publishedRun: false,
		bareRun:      false,
		corruptRun:   true,
	}
	for runID, want := range expected {
		if got := byRun[runID].MutationPossible(); got != want {
			t.Fatalf("run %s MutationPossible() = %v, want %v", runID, got, want)
		}
	}
}

func TestPublishedStateRoundTrips(t *testing.T) {
	t.Parallel()

	layout := newTestLayout(t)
	source := testSource()
	paths, err := layout.PreparePaths(source, "22222222-2222-4222-8222-222222222222", false)
	if err != nil {
		t.Fatalf("prepare paths: %v", err)
	}

	if _, ok, err := ReadPublishedState(paths.Metadata); err != nil || ok {
		t.Fatalf("a fresh source has no published state (ok=%v err=%v)", ok, err)
	}
	state := PublishedState{SourceID: source.ID, RunID: "run", StateVersion: 3, Files: 7, Bytes: 42}
	if err := WritePublishedState(paths.Metadata, state); err != nil {
		t.Fatalf("write published state: %v", err)
	}
	loaded, ok, err := ReadPublishedState(paths.Metadata)
	if err != nil || !ok {
		t.Fatalf("read published state: ok=%v err=%v", ok, err)
	}
	if loaded.StateVersion != 3 || loaded.Files != 7 || loaded.Bytes != 42 {
		t.Fatalf("published state = %#v", loaded)
	}
}

func TestNewLayoutRejectsAnUnconfiguredRoot(t *testing.T) {
	t.Parallel()

	if _, err := NewLayout("   "); err == nil {
		t.Fatal("an empty staging root must be rejected")
	} else if !strings.Contains(err.Error(), "staging root") {
		t.Fatalf("unexpected error: %v", err)
	}
}
