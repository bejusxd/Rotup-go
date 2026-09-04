package restic

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/process"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
)

// This is the M4 restic mechanism spike. It proves against a real restic what
// the fake-runner tests can only assume: that the flags ROTUP pins are accepted,
// that the JSON shapes it parses are the ones restic actually emits, that a
// repository identity really is stable and readable back, and that the failures
// ROTUP classifies are the failures restic actually produces.
//
// It is opt-in and needs nothing but a restic binary and a temporary directory.
// It creates no loop device, needs no privilege, and touches no host disk:
//
//	ROTUP_RESTIC_SPIKE=1 ROTUP_RESTIC_PATH=$(command -v restic) \
//	  go test ./internal/adapters/restic/ -run Spike -v
const (
	spikeEnv       = "ROTUP_RESTIC_SPIKE"
	spikePathEnv   = "ROTUP_RESTIC_PATH"
	spikePassword  = "spike-repository-password"
	spikeOtherPass = "a-different-repository-password"
)

// requireResticSpike skips unless the spike was asked for, and fails loudly
// when it was asked for but cannot run. A spike that quietly skips when it was
// explicitly requested would be reported as a pass it never earned.
func requireResticSpike(t *testing.T) string {
	t.Helper()
	if os.Getenv(spikeEnv) == "" {
		t.Skipf("set %s=1 and %s to a restic binary to exercise the real engine",
			spikeEnv, spikePathEnv)
	}
	path := strings.TrimSpace(os.Getenv(spikePathEnv))
	if path == "" {
		discovered, err := exec.LookPath("restic")
		if err != nil {
			t.Fatalf("%s is set but no restic binary was found: %v", spikeEnv, err)
		}
		path = discovered
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the configured restic binary is not usable: %v", err)
	}
	return path
}

// spikeEngine builds the real adapter over the real process runner and the real
// secret store, so the whole password path is exercised.
func spikeEngine(t *testing.T, password string) (*Engine, ports.RepositorySpec, string) {
	t.Helper()
	binary := requireResticSpike(t)
	root := t.TempDir()

	store, err := secrets.NewFileStore(filepath.Join(root, "secrets"))
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	locator, err := store.Put(context.Background(), domain.SecretPurposeRepositoryPassword, password)
	if err != nil {
		t.Fatalf("store password: %v", err)
	}

	engine := New(Config{
		ExecutablePath: binary,
		InspectTimeout: 2 * time.Minute,
		BackupTimeout:  5 * time.Minute,
		CheckTimeout:   5 * time.Minute,
	}, process.NewRunner(), store, store, nil)

	directory := filepath.Join(root, "media", "rotup", "restic")
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		t.Fatalf("create repository parent: %v", err)
	}
	return engine, ports.RepositorySpec{
		Directory:  directory,
		Credential: ports.CredentialHandle{Locator: locator},
	}, root
}

// A. A real repository is initialized and its identity read back.
func TestSpikeInitializeAndReadIdentity(t *testing.T) {
	engine, spec, _ := spikeEngine(t, spikePassword)
	ctx := context.Background()

	info, err := engine.InitializeRepository(ctx, spec)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := domain.ValidateResticRepositoryID(info.ID); err != nil {
		t.Fatalf("restic reported an identity ROTUP does not accept: %v (%q)", err, info.ID)
	}
	t.Logf("repository identity %s, format version %d", info.ID, info.Version)

	// Reopening must report the same identity. If this were not stable, every
	// identity check ROTUP performs would be meaningless.
	reopened, err := engine.InspectRepository(ctx, spec)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.ID != info.ID {
		t.Fatalf("repository identity changed between opens: %q then %q", info.ID, reopened.ID)
	}
}

// B and C. A first backup produces a snapshot that the listing confirms, and a
// second backup of changed staging produces a new one without disturbing it.
func TestSpikeBackupsProduceVerifiableSnapshots(t *testing.T) {
	engine, spec, root := spikeEngine(t, spikePassword)
	ctx := context.Background()
	if _, err := engine.InitializeRepository(ctx, spec); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	staging := filepath.Join(root, "staging", "current")
	writeSpikeFile(t, filepath.Join(staging, "document.txt"), "first revision")

	firstTag := "rotup-run:11111111-1111-4111-8111-111111111111"
	first, err := engine.Backup(ctx, ports.BackupRequest{
		Repository: spec, Paths: []string{staging},
		Tags: []string{domain.TagROTUP, firstTag},
	})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if first.Stats.SnapshotID == "" || !first.Stats.Available {
		t.Fatalf("restic did not produce a usable summary: %+v", first.Stats)
	}
	t.Logf("first snapshot %s: %d files, %d bytes processed, %d bytes added",
		first.Stats.SnapshotID, first.Stats.FilesProcessed,
		first.Stats.BytesProcessed, first.Stats.BytesAdded)

	snapshots, err := engine.Snapshots(ctx, spec)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if !containsSnapshot(snapshots, first.Stats.SnapshotID, firstTag) {
		t.Fatalf("the first snapshot is not in the repository listing: %+v", snapshots)
	}

	// C. Changed staging produces a second snapshot; the first survives.
	writeSpikeFile(t, filepath.Join(staging, "document.txt"), "second revision, longer")
	writeSpikeFile(t, filepath.Join(staging, "added.txt"), "new file")
	secondTag := "rotup-run:22222222-2222-4222-8222-222222222222"
	second, err := engine.Backup(ctx, ports.BackupRequest{
		Repository: spec, Paths: []string{staging},
		Tags: []string{domain.TagROTUP, secondTag},
	})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if second.Stats.SnapshotID == first.Stats.SnapshotID {
		t.Fatal("the second backup reused the first snapshot identifier")
	}

	snapshots, err = engine.Snapshots(ctx, spec)
	if err != nil {
		t.Fatalf("list snapshots again: %v", err)
	}
	if !containsSnapshot(snapshots, first.Stats.SnapshotID, firstTag) {
		t.Fatal("the first snapshot disappeared after a second backup")
	}
	if !containsSnapshot(snapshots, second.Stats.SnapshotID, secondTag) {
		t.Fatal("the second snapshot is not in the repository listing")
	}
}

// E. A wrong password fails safely and mutates nothing.
func TestSpikeWrongPasswordIsRefused(t *testing.T) {
	engine, spec, root := spikeEngine(t, spikePassword)
	ctx := context.Background()
	if _, err := engine.InitializeRepository(ctx, spec); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	before := countEntries(t, spec.Directory)

	// A second engine with a different stored password, pointed at the same
	// repository.
	other, otherSpec, _ := spikeEngine(t, spikeOtherPass)
	otherSpec.Directory = spec.Directory

	if _, err := other.InspectRepository(ctx, otherSpec); err == nil {
		t.Fatal("a repository was opened with the wrong password")
	} else if class := domain.ClassOf(err); class != domain.ErrClassAuthentication {
		t.Fatalf("wrong password was classified as %s, not AUTHENTICATION: %v", class, err)
	}

	if _, err := other.Backup(ctx, ports.BackupRequest{
		Repository: otherSpec, Paths: []string{root}, Tags: []string{domain.TagROTUP},
	}); err == nil {
		t.Fatal("a backup ran against a repository the password does not open")
	}
	if after := countEntries(t, spec.Directory); after != before {
		t.Fatalf("the repository changed after a refused operation: %d entries then %d", before, after)
	}
}

// F. A location that is not a repository is refused rather than adopted.
func TestSpikeMissingRepositoryIsRefused(t *testing.T) {
	engine, spec, _ := spikeEngine(t, spikePassword)
	ctx := context.Background()

	if _, err := engine.InspectRepository(ctx, spec); err == nil {
		t.Fatal("an absent repository was opened")
	} else {
		var typed *domain.Error
		if !domain.AsError(err, &typed) {
			t.Fatalf("an absent repository produced an untyped error: %v", err)
		}
		t.Logf("absent repository classified as %s/%s", typed.Class, typed.Code)
	}

	// A directory full of unrelated files is not a repository either.
	writeSpikeFile(t, filepath.Join(spec.Directory, "notes.txt"), "not a repository")
	if _, err := engine.InspectRepository(ctx, spec); err == nil {
		t.Fatal("a directory of unrelated files was accepted as a repository")
	}
}

// G. A snapshot identifier that is not in the repository is not found. This is
// the read the durability boundary depends on.
func TestSpikeUnknownSnapshotIsNotFound(t *testing.T) {
	engine, spec, root := spikeEngine(t, spikePassword)
	ctx := context.Background()
	if _, err := engine.InitializeRepository(ctx, spec); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	staging := filepath.Join(root, "staging", "current")
	writeSpikeFile(t, filepath.Join(staging, "document.txt"), "content")
	if _, err := engine.Backup(ctx, ports.BackupRequest{
		Repository: spec, Paths: []string{staging}, Tags: []string{domain.TagROTUP},
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	snapshots, err := engine.Snapshots(ctx, spec)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	const invented = "0000000000000000000000000000000000000000000000000000000000000000"
	if containsSnapshot(snapshots, invented, "") {
		t.Fatal("a snapshot identifier that was never created was found in the repository")
	}
}

// A repository check on a real repository must pass and report no errors.
func TestSpikeCheckPassesOnAHealthyRepository(t *testing.T) {
	engine, spec, root := spikeEngine(t, spikePassword)
	ctx := context.Background()
	if _, err := engine.InitializeRepository(ctx, spec); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	staging := filepath.Join(root, "staging", "current")
	writeSpikeFile(t, filepath.Join(staging, "document.txt"), "content")
	if _, err := engine.Backup(ctx, ports.BackupRequest{
		Repository: spec, Paths: []string{staging}, Tags: []string{domain.TagROTUP},
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	result, err := engine.Check(ctx, spec)
	if err != nil {
		t.Fatalf("check a healthy repository: %v (%s)", err, result.Diagnostics)
	}
	if result.Errors != 0 {
		t.Fatalf("a healthy repository reported %d check error(s)", result.Errors)
	}
	t.Logf("repository check completed in %s", result.Duration.Round(time.Millisecond))
}

// D. A cancelled backup is reported as a cancellation, never as a success.
func TestSpikeCancelledBackupIsNotASuccess(t *testing.T) {
	engine, spec, root := spikeEngine(t, spikePassword)
	if _, err := engine.InitializeRepository(context.Background(), spec); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	staging := filepath.Join(root, "staging", "current")
	// Enough data that the backup is still running when the context is cancelled.
	for i := 0; i < 400; i++ {
		writeSpikeFile(t, filepath.Join(staging, "file-"+itoa(i)+".bin"),
			strings.Repeat("rotup-spike-payload", 4096))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	observed := make(chan bool, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			entries, err := os.ReadDir(filepath.Join(spec.Directory, "locks"))
			if err == nil && len(entries) > 0 {
				cancel()
				observed <- true
				return
			}
			select {
			case <-ctx.Done():
				observed <- false
				return
			case <-ticker.C:
			}
		}
	}()
	result, err := engine.Backup(ctx, ports.BackupRequest{
		Repository: spec, Paths: []string{staging}, Tags: []string{domain.TagROTUP},
	})
	cancel()
	if !<-observed {
		t.Fatal("could not observe the real subprocess before cancellation")
	}
	if err == nil {
		t.Fatal("the real subprocess was not interrupted")
	}
	class := domain.ClassOf(err)
	if class != domain.ErrClassCancellation && class != domain.ErrClassTimeout {
		t.Fatalf("an interrupted backup was classified as %s: %v", class, err)
	}
	if result.Stats.SnapshotID != "" {
		t.Fatalf("an interrupted backup reported snapshot %q", result.Stats.SnapshotID)
	}
	t.Logf("interrupted backup classified as %s", class)
}

func containsSnapshot(snapshots []ports.Snapshot, id, requiredTag string) bool {
	for _, snapshot := range snapshots {
		if snapshot.ID != id && snapshot.ShortID != id {
			continue
		}
		if requiredTag != "" && !snapshot.HasTag(requiredTag) {
			continue
		}
		return true
	}
	return false
}

func writeSpikeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func countEntries(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(dir, func(string, os.FileInfo, error) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return count
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
