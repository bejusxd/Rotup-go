package restic

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
)

const testPassword = "correct-horse-battery-staple"

// fakeRunner records what the adapter asked for and returns scripted results,
// one per invocation.
type fakeRunner struct {
	results []ports.ProcessResult
	calls   []ports.CommandSpec
	// passwordFiles records the contents of the password file as it existed at
	// the moment of each call, so a test can prove the secret really was on
	// disk while restic ran and gone afterwards.
	passwordFiles []string
	paths         []string
}

func (r *fakeRunner) Run(_ context.Context, spec ports.CommandSpec) ports.ProcessResult {
	r.calls = append(r.calls, spec)
	if path := flagValue(spec.Args, "--password-file"); path != "" {
		r.paths = append(r.paths, path)
		contents, err := os.ReadFile(path)
		if err == nil {
			r.passwordFiles = append(r.passwordFiles, string(contents))
		} else {
			r.passwordFiles = append(r.passwordFiles, "<unreadable>")
		}
	}
	if len(r.results) == 0 {
		return ports.ProcessResult{Started: true, Exited: true, ExitCode: 0}
	}
	next := r.results[0]
	r.results = r.results[1:]
	return next
}

func flagValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func succeed(stdout string) ports.ProcessResult {
	return ports.ProcessResult{Started: true, Exited: true, ExitCode: 0, Stdout: []byte(stdout)}
}

func fail(code int, stderr string) ports.ProcessResult {
	return ports.ProcessResult{Started: true, Exited: true, ExitCode: code, Stderr: []byte(stderr)}
}

const testRepositoryID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testSnapshotID = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func configJSON() string {
	return `{"version":2,"id":"` + testRepositoryID + `","chunker_polynomial":"25b468838dcb75"}`
}

func summaryJSON(snapshotID string) string {
	return `{"message_type":"summary","files_new":3,"files_changed":1,"files_unmodified":7,` +
		`"total_files_processed":11,"total_bytes_processed":4096,"data_added":2048,` +
		`"snapshot_id":"` + snapshotID + `"}`
}

func snapshotsJSON(snapshotID string, tags ...string) string {
	quoted := make([]string, 0, len(tags))
	for _, tag := range tags {
		quoted = append(quoted, `"`+tag+`"`)
	}
	return `[{"time":"2026-09-04T10:00:00.123456Z","tree":"abc","paths":["/staging/current"],` +
		`"hostname":"backup-host","tags":[` + strings.Join(quoted, ",") + `],` +
		`"id":"` + snapshotID + `","short_id":"` + snapshotID[:8] + `"}]`
}

// testEngine builds an adapter over a fake runner and a real secret store, so
// the password path is exercised end to end rather than stubbed out.
func testEngine(t *testing.T, results ...ports.ProcessResult) (*Engine, *fakeRunner, ports.RepositorySpec) {
	t.Helper()

	store, err := secrets.NewFileStore(filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatalf("build secret store: %v", err)
	}
	locator, err := store.Put(context.Background(), domain.SecretPurposeRepositoryPassword, testPassword)
	if err != nil {
		t.Fatalf("store repository password: %v", err)
	}
	runner := &fakeRunner{results: results}
	engine := New(Config{ExecutablePath: filepath.Join(t.TempDir(), "restic")}, runner, store, store, nil)

	spec := ports.RepositorySpec{
		Directory:  filepath.Join(t.TempDir(), "repo"),
		Credential: ports.CredentialHandle{Locator: locator},
	}
	return engine, runner, spec
}

func TestProbeParsesVersion(t *testing.T) {
	t.Parallel()

	engine, _, _ := testEngine(t, succeed("restic 0.17.3 compiled with go1.23.3 on linux/amd64\n"))
	info, err := engine.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Version != "0.17.3" || info.Name != "restic" {
		t.Fatalf("probe returned %+v", info)
	}
}

func TestProbeRefusesUnparsableVersion(t *testing.T) {
	t.Parallel()

	engine, _, _ := testEngine(t, succeed("something else entirely\n"))
	if _, err := engine.Probe(context.Background()); err == nil {
		t.Fatal("an unparsable version was accepted")
	}
}

// The repository identity has to come from the repository, not from the fact
// that init exited zero.
func TestInitializeReadsIdentityBack(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t,
		succeed("created restic repository\n"),
		succeed(configJSON()),
	)
	info, err := engine.InitializeRepository(context.Background(), spec)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if info.ID != testRepositoryID {
		t.Fatalf("repository id = %q", info.ID)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected an init and a config read, got %d calls", len(runner.calls))
	}
	if runner.calls[0].Args[0] != "init" {
		t.Fatalf("first call was %v", runner.calls[0].Args)
	}
	if runner.calls[1].Args[0] != "cat" || runner.calls[1].Args[1] != "config" {
		t.Fatalf("second call was %v", runner.calls[1].Args)
	}
}

func TestInitializeFailsWhenIdentityUnreadable(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t,
		succeed("created restic repository\n"),
		succeed("not json at all"),
	)
	_, err := engine.InitializeRepository(context.Background(), spec)
	if err == nil {
		t.Fatal("a repository whose identity could not be read was accepted")
	}
	if code := codeOf(t, err); code != domain.RepositoryErrorUnreadable {
		t.Fatalf("error code = %q", code)
	}
}

func TestInspectMapsWrongPassword(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, fail(1, "Fatal: wrong password or no key found"))
	_, err := engine.InspectRepository(context.Background(), spec)
	if err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if class := domain.ClassOf(err); class != domain.ErrClassAuthentication {
		t.Fatalf("error class = %s, want AUTHENTICATION", class)
	}
}

func TestBackupSucceedsAndParsesSummary(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t, succeed(
		`{"message_type":"status","percent_done":0.5}`+"\n"+summaryJSON(testSnapshotID)+"\n"))

	result, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
		Tags:       []string{domain.TagROTUP, domain.RunTag("11111111-2222-4333-8444-555555555555")},
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if result.Stats.SnapshotID != testSnapshotID {
		t.Fatalf("snapshot id = %q", result.Stats.SnapshotID)
	}
	if result.Stats.FilesProcessed != 11 || result.Stats.BytesProcessed != 4096 || result.Stats.BytesAdded != 2048 {
		t.Fatalf("counters = %+v", result.Stats)
	}

	args := runner.calls[0].Args
	if !hasFlag(args, "--json") || !hasFlag(args, "--no-cache") {
		t.Fatalf("backup args are missing a pinned flag: %v", args)
	}
	if !hasFlag(args, "--") {
		t.Fatalf("backup args have no path separator: %v", args)
	}
	// restic implements --one-file-system on Unix only, and refuses to start
	// when it is passed on Windows. The production target keeps the guarantee;
	// the development build cannot have it.
	if hasFlag(args, "--one-file-system") != (runtime.GOOS != "windows") {
		t.Fatalf("--one-file-system does not match what this platform supports: %v", args)
	}
}

// A non-zero exit is never a success, whatever the parsed output says.
func TestBackupNonZeroExitFails(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, ports.ProcessResult{
		Started: true, Exited: true, ExitCode: 3,
		Stdout: []byte(summaryJSON(testSnapshotID)),
		Stderr: []byte("error: could not read file"),
	})
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil {
		t.Fatal("restic exit code 3 with a summary was reported as a success")
	}
	if code := codeOf(t, err); code != "restic_source_unreadable" {
		t.Fatalf("error code = %q", code)
	}
}

// A clean exit whose output does not establish a snapshot must fail closed.
func TestBackupWithoutSummaryFails(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, succeed(`{"message_type":"status","percent_done":1}`))
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil {
		t.Fatal("a backup with no summary was reported as a success")
	}
	if code := codeOf(t, err); code != "restic_reported_incomplete" {
		t.Fatalf("error code = %q", code)
	}
}

func TestBackupWithUnreadableOutputFails(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, succeed("this is not machine readable at all"))
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil {
		t.Fatal("a backup with unusable output was reported as a success")
	}
}

// restic exiting zero while its own output reports errors is the M2 lesson
// applied to the snapshot engine.
func TestBackupWithReportedErrorsFails(t *testing.T) {
	t.Parallel()

	stdout := `{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/x"}` + "\n" +
		summaryJSON(testSnapshotID) + "\n"
	engine, _, spec := testEngine(t, succeed(stdout))
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil {
		t.Fatal("a backup whose output reported errors was accepted")
	}
	if code := codeOf(t, err); code != "restic_reported_incomplete" {
		t.Fatalf("error code = %q", code)
	}
}

func TestBackupWithStderrReadErrorsFails(t *testing.T) {
	t.Parallel()
	result := succeed(summaryJSON(testSnapshotID))
	result.Stderr = []byte(`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/x"}`)
	engine, _, spec := testEngine(t, result)
	backup, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec, Paths: []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil || backup.Stats.Errors != 1 {
		t.Fatal("stderr read errors were hidden by a successful exit and stdout summary")
	}
}

func TestBackupRefusesUnsafeTag(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t)
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
		Tags:       []string{"--force"},
	})
	if err == nil {
		t.Fatal("a flag-like tag was accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatal("restic was invoked with an unsafe tag")
	}
}

func TestBackupRefusesRelativePath(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t)
	_, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{"relative/path"},
	})
	if err == nil {
		t.Fatal("a relative backup path was accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatal("restic was invoked with a relative path")
	}
}

func TestSnapshotsParsesListing(t *testing.T) {
	t.Parallel()

	tag := domain.RunTag("11111111-2222-4333-8444-555555555555")
	engine, _, spec := testEngine(t, succeed(snapshotsJSON(testSnapshotID, domain.TagROTUP, tag)))
	snapshots, err := engine.Snapshots(context.Background(), spec)
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots", len(snapshots))
	}
	if !snapshots[0].HasTag(tag) {
		t.Fatalf("snapshot tags = %v", snapshots[0].Tags)
	}
	if snapshots[0].Time.IsZero() {
		t.Fatal("snapshot time was not parsed")
	}
}

// An unreadable listing must not look like an empty repository: that difference
// is what a snapshot verification depends on.
func TestSnapshotsRefusesUnreadableListing(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, succeed("not json"))
	if _, err := engine.Snapshots(context.Background(), spec); err == nil {
		t.Fatal("an unreadable snapshot listing was reported as an empty repository")
	}
}

func TestSnapshotsRefusesTruncatedListing(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, ports.ProcessResult{
		Started: true, Exited: true, ExitCode: 0,
		Stdout:          []byte(snapshotsJSON(testSnapshotID, domain.TagROTUP)),
		StdoutTruncated: true,
	})
	if _, err := engine.Snapshots(context.Background(), spec); err == nil {
		t.Fatal("a truncated snapshot listing was accepted")
	}
}

func TestCheckReportsErrors(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, succeed(
		`{"message_type":"error","message":"pack is damaged"}`+"\n"))
	result, err := engine.Check(context.Background(), spec)
	if err == nil {
		t.Fatal("a check that reported an error was accepted")
	}
	if result.Errors != 1 {
		t.Fatalf("check errors = %d", result.Errors)
	}
}

func TestCheckSucceedsCleanly(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, succeed(
		`{"message_type":"status","percent_done":1}`+"\n"))
	result, err := engine.Check(context.Background(), spec)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Errors != 0 {
		t.Fatalf("check errors = %d", result.Errors)
	}
}

// The password must be readable by restic while it runs, must never be an
// argument, and must be gone once the operation ends.
func TestPasswordIsAFileAndIsCleanedUp(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t, succeed(configJSON()))
	if _, err := engine.InspectRepository(context.Background(), spec); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	if len(runner.passwordFiles) != 1 || runner.passwordFiles[0] != testPassword {
		t.Fatalf("the password file did not hold the password during the call: %v", runner.passwordFiles)
	}
	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, testPassword) {
				t.Fatal("the repository password appeared in an argument")
			}
		}
		for _, value := range call.Env {
			if strings.Contains(value, testPassword) {
				t.Fatal("the repository password appeared in the child environment")
			}
		}
	}
	for _, path := range runner.paths {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("the transient password file %s still exists after the operation", path)
		}
	}
}

// Diagnostics are what reach history and logs, so an engine that echoes the
// password must not be able to leak it there.
func TestDiagnosticsRedactThePassword(t *testing.T) {
	t.Parallel()

	engine, _, spec := testEngine(t, ports.ProcessResult{
		Started: true, Exited: true, ExitCode: 1,
		Stderr: []byte("Fatal: could not open repository with password " + testPassword),
	})
	result, err := engine.Backup(context.Background(), ports.BackupRequest{
		Repository: spec,
		Paths:      []string{filepath.Join(t.TempDir(), "current")},
	})
	if err == nil {
		t.Fatal("a failing backup was accepted")
	}
	if strings.Contains(result.Diagnostics, testPassword) {
		t.Fatal("the repository password appeared in the retained diagnostics")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("the repository password appeared in the error message")
	}
}

func TestOperationsRefuseWithoutPassword(t *testing.T) {
	t.Parallel()

	engine, runner, spec := testEngine(t)
	spec.Credential.Locator = ""
	if _, err := engine.InspectRepository(context.Background(), spec); err == nil {
		t.Fatal("a repository with no stored password was opened")
	}
	if len(runner.calls) != 0 {
		t.Fatal("restic was invoked without a password")
	}
}

func TestUnconfiguredEngineRefuses(t *testing.T) {
	t.Parallel()

	engine := New(Config{}, &fakeRunner{}, nil, nil, nil)
	if engine.Available() {
		t.Fatal("an engine with no executable reported itself available")
	}
	if _, err := engine.Probe(context.Background()); err == nil {
		t.Fatal("an unconfigured engine probed successfully")
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var typed *domain.Error
	if !domain.AsError(err, &typed) {
		t.Fatalf("error %v is not a typed ROTUP error", err)
	}
	return typed.Code
}
