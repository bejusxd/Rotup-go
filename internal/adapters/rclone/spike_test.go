package rclone

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/platform/process"
	"github.com/bejusxd/rotup/internal/ports"
)

// This file is the archive_then_sync safety spike the architecture asks for in
// M2. It runs a real rclone binary and asserts the engine semantics ROTUP's
// staging model depends on, rather than assuming them.
//
// It is opt-in because rclone is a separately installed tool that ROTUP never
// downloads. Run it with:
//
//	ROTUP_RCLONE_SPIKE=1 go test ./internal/adapters/rclone/ -run Spike -v
//
// and optionally ROTUP_RCLONE_PATH=/path/to/rclone. The source side uses a
// local directory rather than an SMB share: the properties under test are
// rclone's own copy, sync, backup-dir and delete-after semantics, which are
// backend-independent. SMB-specific behaviour still needs a real share and is
// verified on the Linux target during deployment.
//
// Everything here uses the same pinned flag set that production builds, by
// calling collectArgs and substituting only the source argument.

const spikeEnv = "ROTUP_RCLONE_SPIKE"

// spikeBinary resolves the rclone binary or skips the test.
func spikeBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv(spikeEnv) == "" {
		t.Skipf("set %s=1 (and optionally ROTUP_RCLONE_PATH) to run the real rclone safety spike", spikeEnv)
	}
	if configured := strings.TrimSpace(os.Getenv("ROTUP_RCLONE_PATH")); configured != "" {
		return configured
	}
	path, err := exec.LookPath("rclone")
	if err != nil {
		t.Fatalf("%s is set but no rclone binary was found: %v", spikeEnv, err)
	}
	return path
}

type spikeEnvironment struct {
	t       *testing.T
	binary  string
	source  string
	current string
	archive string
	work    string
	config  string
}

func newSpikeEnvironment(t *testing.T) *spikeEnvironment {
	t.Helper()
	binary := spikeBinary(t)

	root := t.TempDir()
	env := &spikeEnvironment{
		t:       t,
		binary:  binary,
		source:  filepath.Join(root, "source"),
		current: filepath.Join(root, "current"),
		archive: filepath.Join(root, "revisions"),
		work:    filepath.Join(root, "work"),
		config:  filepath.Join(root, "rclone.conf"),
	}
	for _, dir := range []string{env.source, env.current, env.work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	// An empty configuration keeps rclone from reading the developer's own.
	if err := os.WriteFile(env.config, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty config: %v", err)
	}
	return env
}

func (e *spikeEnvironment) write(dir, relative, content string) {
	e.t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatalf("create directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatalf("write file: %v", err)
	}
	// rclone compares size and modification time by default, so an update has
	// to look newer than what it replaces.
	stamp := time.Now().Add(time.Duration(len(content)) * time.Second)
	_ = os.Chtimes(path, stamp, stamp)
}

func (e *spikeEnvironment) tree(dir string) map[string]string {
	e.t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
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
		e.t.Fatalf("read tree %s: %v", dir, err)
	}
	return result
}

// run executes the production argument array with the local source substituted
// for the SMB remote, and returns the process result.
func (e *spikeEnvironment) run(mode ports.TransferMode) ports.ProcessResult {
	e.t.Helper()

	request := ports.TransferRequest{
		Mode:           mode,
		DestinationDir: e.current,
		TempDir:        e.work,
	}
	if mode == ports.TransferSyncWithArchive {
		request.ArchiveDir = e.archive
	}
	session := &session{
		spec:       ports.SourceSpec{Share: "unused"},
		configPath: e.config,
		cacheDir:   e.work,
		redactor:   nil,
	}
	args := collectArgs(request, session)
	// The remote is the second-to-last argument; swap in the local directory.
	args[len(args)-2] = e.source

	return process.NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: e.binary,
		Args: args,
		Env:  minimalEnvironment(),
	})
}

func (e *spikeEnvironment) assertTree(label, dir string, want map[string]string) {
	e.t.Helper()
	got := e.tree(dir)
	if len(got) != len(want) {
		e.t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for name, content := range want {
		if got[name] != content {
			e.t.Fatalf("%s[%s] = %q, want %q (full tree %v)", label, name, got[name], content, got)
		}
	}
}

func TestSpikeRecordsTheRcloneVersion(t *testing.T) {
	binary := spikeBinary(t)
	result := process.NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: binary, Args: []string{"version"}, Env: minimalEnvironment(),
	})
	if result.ExitCode != 0 {
		t.Fatalf("rclone version failed: %s", result.Stderr)
	}
	version := parseVersion(string(result.Stdout))
	if version == "" {
		t.Fatalf("could not parse a version from: %s", result.Stdout)
	}
	t.Logf("SPIKE: verified against rclone %s at %s", version, binary)
}

// Scenario A: an initial collection populates current and archives nothing.
func TestSpikeInitialCollection(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "b.txt", "B")

	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("sync failed (%d): %s", result.ExitCode, result.Stderr)
	}
	env.assertTree("current", env.current, map[string]string{"a.txt": "A", "b.txt": "B"})
	env.assertTree("revisions", env.archive, map[string]string{})
}

// Scenario B: a replaced file must survive in the revision batch.
func TestSpikeReplacedFileIsArchived(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "OLD")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("seed sync failed: %s", result.Stderr)
	}

	env.write(env.source, "a.txt", "NEWER")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("sync failed (%d): %s", result.ExitCode, result.Stderr)
	}

	env.assertTree("current", env.current, map[string]string{"a.txt": "NEWER"})
	env.assertTree("revisions", env.archive, map[string]string{"a.txt": "OLD"})
}

// Scenario C: a deleted source file must be archived, never simply removed.
func TestSpikeDeletedFileIsArchived(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "b.txt", "B")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("seed sync failed: %s", result.Stderr)
	}

	if err := os.Remove(filepath.Join(env.source, "b.txt")); err != nil {
		t.Fatalf("remove source file: %v", err)
	}
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("sync failed (%d): %s", result.ExitCode, result.Stderr)
	}

	env.assertTree("current", env.current, map[string]string{"a.txt": "A"})
	env.assertTree("revisions", env.archive, map[string]string{"b.txt": "B"})
}

// copy_only must never remove anything, even when the source has.
func TestSpikeCopyModeNeverDeletes(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "b.txt", "B")
	if result := env.run(ports.TransferCopy); result.ExitCode != 0 {
		t.Fatalf("seed copy failed: %s", result.Stderr)
	}

	if err := os.Remove(filepath.Join(env.source, "b.txt")); err != nil {
		t.Fatalf("remove source file: %v", err)
	}
	if result := env.run(ports.TransferCopy); result.ExitCode != 0 {
		t.Fatalf("copy failed (%d): %s", result.ExitCode, result.Stderr)
	}
	env.assertTree("current", env.current, map[string]string{"a.txt": "A", "b.txt": "B"})
}

// Scenario D: an unreadable source must not empty the destination. rclone's own
// refusal is the first line of defence behind ROTUP's preflight guard.
func TestSpikeMissingSourceDoesNotEmptyCurrent(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("seed sync failed: %s", result.Stderr)
	}
	if err := os.RemoveAll(env.source); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	result := env.run(ports.TransferSyncWithArchive)
	if result.ExitCode == 0 {
		t.Fatal("rclone reported success for a source directory that does not exist")
	}
	env.assertTree("current", env.current, map[string]string{"a.txt": "A"})

	failure := classify("the collection", result, string(result.Stderr))
	if failure == nil {
		t.Fatal("a missing source must classify as a typed failure")
	}
	t.Logf("SPIKE: missing source classified as %s/%s", failure.Class, failure.Code)
}

// Scenario E, at the engine level: an empty source really would empty current,
// which is exactly why ROTUP's preflight guard blocks the run before the engine
// is ever started. This test documents the danger the guard exists to prevent.
func TestSpikeEmptySourceWouldEmptyCurrentWithoutTheGuard(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("seed sync failed: %s", result.Stderr)
	}
	if err := os.Remove(filepath.Join(env.source, "a.txt")); err != nil {
		t.Fatalf("empty the source: %v", err)
	}

	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("sync failed: %s", result.Stderr)
	}
	env.assertTree("current", env.current, map[string]string{})
	// The data is not lost, because --backup-dir archived it rather than
	// deleting it. ROTUP still refuses to run this at all.
	env.assertTree("revisions", env.archive, map[string]string{"a.txt": "A"})
	t.Log("SPIKE: confirmed that an empty source empties current; ROTUP's preflight guard must block this")
}

// Scenario G: a source file that cannot be read must be reported, and must not
// cause the deletion phase to run.
func TestSpikeUnreadableFileIsReportedAndBlocksDeletion(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root defeats the permission-based failure this test needs")
	}
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "b.txt", "B")
	if result := env.run(ports.TransferSyncWithArchive); result.ExitCode != 0 {
		t.Fatalf("seed sync failed: %s", result.Stderr)
	}

	// Change one file and make it unreadable, and delete another.
	env.write(env.source, "a.txt", "UPDATED")
	if err := os.Chmod(filepath.Join(env.source, "a.txt"), 0o000); err != nil {
		t.Skipf("this platform does not support removing read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(env.source, "a.txt"), 0o600) })
	if err := os.Remove(filepath.Join(env.source, "b.txt")); err != nil {
		t.Fatalf("remove source file: %v", err)
	}

	result := env.run(ports.TransferSyncWithArchive)
	if result.ExitCode == 0 {
		t.Skip("this platform still allowed the unreadable file to be read")
	}

	parsed := parseLogOutput(string(result.Stderr))
	if !parsed.Stats.Available {
		t.Fatalf("no machine-readable statistics in engine output: %s", result.Stderr)
	}
	if parsed.Stats.Errors == 0 {
		t.Fatalf("the engine reported no errors despite failing: %#v", parsed.Stats)
	}
	// --delete-after means the deletion phase is skipped when transfers failed,
	// so the file that disappeared at the source is still staged.
	current := env.tree(env.current)
	if _, stillThere := current["b.txt"]; !stillThere {
		t.Fatal("deletions must not run after a failed transfer phase")
	}
	t.Logf("SPIKE: transfer errors=%d, deletes=%d, current=%v", parsed.Stats.Errors, parsed.Stats.Deletes, current)
}

// The size probe must produce the machine-readable output the empty-source
// guard depends on.
func TestSpikeSizeProbeIsMachineReadable(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "AAAA")
	env.write(env.source, "nested/b.txt", "BB")

	result := process.NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: env.binary,
		Args: []string{"size", "--json", "--config", env.config, env.source},
		Env:  minimalEnvironment(),
	})
	if result.ExitCode != 0 {
		t.Fatalf("size failed: %s", result.Stderr)
	}
	var parsed sizeOutput
	if err := json.Unmarshal(trimToJSONObject(result.Stdout), &parsed); err != nil {
		t.Fatalf("size output is not the shape ROTUP parses (%v): %s", err, result.Stdout)
	}
	if parsed.Count != 2 || parsed.Bytes != 6 {
		t.Fatalf("size = %#v, want 2 files of 6 bytes", parsed)
	}
}

// The listing probe used by the connection test must parse too.
func TestSpikeListingProbeIsMachineReadable(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "nested/b.txt", "B")

	result := process.NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: env.binary,
		Args: []string{"lsjson", "--max-depth", "1", "--config", env.config, env.source},
		Env:  minimalEnvironment(),
	})
	if result.ExitCode != 0 {
		t.Fatalf("lsjson failed: %s", result.Stderr)
	}
	var entries []listingEntry
	if err := json.Unmarshal(result.Stdout, &entries); err != nil {
		t.Fatalf("lsjson output is not the shape ROTUP parses (%v): %s", err, result.Stdout)
	}
	if len(entries) != 2 {
		t.Fatalf("listed %d top-level entries, want 2", len(entries))
	}
}

// The statistics ROTUP relies on to decide whether a failed run may have
// mutated staging must actually be emitted by the pinned flag set.
func TestSpikeCollectEmitsParsableStatistics(t *testing.T) {
	env := newSpikeEnvironment(t)
	env.write(env.source, "a.txt", "A")
	env.write(env.source, "b.txt", "BB")

	result := env.run(ports.TransferSyncWithArchive)
	if result.ExitCode != 0 {
		t.Fatalf("sync failed: %s", result.Stderr)
	}
	parsed := parseLogOutput(string(result.Stderr))
	if !parsed.Stats.Available {
		t.Fatalf("the pinned flags produced no machine-readable statistics: %s", result.Stderr)
	}
	if parsed.Stats.Transfers != 2 {
		t.Fatalf("transfers = %d, want 2 (%#v)", parsed.Stats.Transfers, parsed.Stats)
	}
	if !parsed.Stats.MutationObserved() {
		t.Fatal("a run that transferred files must report an observed mutation")
	}

	// A run that changes nothing must be distinguishable from one that does.
	second := env.run(ports.TransferSyncWithArchive)
	if second.ExitCode != 0 {
		t.Fatalf("second sync failed: %s", second.Stderr)
	}
	idle := parseLogOutput(string(second.Stderr))
	if !idle.Stats.Available {
		t.Fatalf("no statistics for the idle run: %s", second.Stderr)
	}
	if idle.Stats.MutationObserved() {
		t.Fatalf("an idle run must not report a mutation: %#v", idle.Stats)
	}
}

// ROTUP implements rclone's password obfuscation itself so the plaintext never
// reaches an argument vector. That implementation has to agree with the binary.
func TestSpikeObscureMatchesTheRealBinary(t *testing.T) {
	binary := spikeBinary(t)
	// A throwaway value: `rclone reveal` takes the obscured form as an
	// argument, so no real secret is ever placed on a command line here.
	const sample = "spike-only-not-a-real-password"

	obscured, err := obscure(sample)
	if err != nil {
		t.Fatalf("obscure: %v", err)
	}
	result := process.NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: binary, Args: []string{"reveal", obscured}, Env: minimalEnvironment(),
	})
	if result.ExitCode != 0 {
		t.Fatalf("rclone could not read ROTUP's obscured form (%d): %s", result.ExitCode, result.Stderr)
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != sample {
		t.Fatalf("rclone revealed %q, want %q", got, sample)
	}
	t.Log("SPIKE: ROTUP's obscure implementation is accepted by the real rclone binary")
}
