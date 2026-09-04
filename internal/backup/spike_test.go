package backup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/restic"
	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/process"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
	"github.com/bejusxd/rotup/internal/staging"
)

// This is the M4 backup vertical-slice spike. It runs the real backup service
// against a real restic binary, so the whole path from a staging tree to a
// verified snapshot is exercised end to end: the read-set is captured, the
// repository identity is proven, restic writes, the snapshot is confirmed by a
// separate read, and only then is the run recorded as a success.
//
// The media half is a directory standing in for a mounted disk. That is the
// deliberate division of labour: the Linux spike proves the real mount, sync
// and unmount lifecycle on real media, and this one proves everything above it
// without needing privilege. Neither is a substitute for the other, and a green
// run here is not evidence about hardware.
//
// It is opt-in and touches nothing outside its own temporary directory:
//
//	ROTUP_RESTIC_SPIKE=1 ROTUP_RESTIC_PATH=/path/to/restic \
//	  go test ./internal/backup/ -run Spike -v
const (
	spikeEnv     = "ROTUP_RESTIC_SPIKE"
	spikePathEnv = "ROTUP_RESTIC_PATH"
)

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

// newSpikeHarness is the ordinary harness with the real restic adapter in place
// of the scripted engine.
func newSpikeHarness(t *testing.T) *harness {
	t.Helper()
	binary := requireResticSpike(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()

	store, err := sqlite.Open(ctx, filepath.Join(root, "rotup.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	layout, err := staging.NewLayout(filepath.Join(root, "staging"))
	if err != nil {
		t.Fatalf("staging layout: %v", err)
	}
	secretStore, err := secrets.NewFileStore(filepath.Join(root, "secrets"))
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}

	h := &harness{
		t:       t,
		store:   store,
		media:   newFakeMedia(t),
		layout:  layout,
		barrier: staging.NewBarrier(),
		secrets: secretStore,
		now:     time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC),
	}
	engine := restic.New(restic.Config{
		ExecutablePath: binary,
		InspectTimeout: 3 * time.Minute,
		BackupTimeout:  10 * time.Minute,
		CheckTimeout:   10 * time.Minute,
	}, process.NewRunner(), secretStore, secretStore, logger)

	h.service = New(ctx, Options{
		Store:     store,
		Engine:    engine,
		Media:     h.media,
		Layout:    layout,
		Barrier:   h.barrier,
		Secrets:   secretStore,
		Logger:    logger,
		RunInline: true,
		Now:       func() time.Time { h.now = h.now.Add(time.Second); return h.now },
	})
	return h
}

// spikeInspector builds a second, independent engine pointed at the same
// repository, so a test can confirm what is really on the media rather than
// asking the service to confirm its own work.
func (h *harness) spikeInspector(t *testing.T) (*restic.Engine, ports.RepositorySpec) {
	t.Helper()
	binary := requireResticSpike(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := restic.New(restic.Config{ExecutablePath: binary}, process.NewRunner(),
		h.secrets, h.secrets, logger)

	ref, err := h.store.RepositoryCredentialRef(context.Background(), h.repository.ID)
	if err != nil {
		t.Fatalf("read repository credential: %v", err)
	}
	return engine, ports.RepositorySpec{
		Directory:  filepath.Join(h.media.mountPoint, "rotup", "restic"),
		Credential: ports.CredentialHandle{Locator: ref.ProviderLocator},
	}
}

// A and B. A repository is established on the media, and a manual backup runs
// the whole path to a durable success with a snapshot that is really there.
func TestSpikeManualBackupReachesDurableSuccess(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	if repository.ResticRepositoryID == "" {
		t.Fatal("the repository was established without a proven identity")
	}
	t.Logf("repository identity %s", repository.ResticRepositoryID)

	source := h.seedSource("documents", 3)
	batch := h.seedRevisionBatch(source, true)

	runID, err := h.service.StartBackup(context.Background(), repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if !run.Status.Successful() {
		t.Fatalf("backup did not succeed: %s / %s", run.Status, run.SafeError)
	}
	backup := h.backupRun(runID)
	if backup.Stage != domain.StageDurableSuccess || backup.Durability != domain.DurabilityNormal {
		t.Fatalf("backup did not reach a normal durable success: %#v", backup)
	}
	t.Logf("snapshot %s: %d files, %d bytes processed, %d bytes added",
		backup.VerifiedSnapshotID, backup.FilesProcessed, backup.BytesProcessed, backup.BytesAdded)

	// H. Independently reopen the repository, as a remount would, and confirm
	// the snapshot is there with this run's tag.
	inspector, spec := h.spikeInspector(t)
	snapshots, err := inspector.Snapshots(context.Background(), spec)
	if err != nil {
		t.Fatalf("reopen the repository: %v", err)
	}
	if findSnapshot(snapshots, backup.VerifiedSnapshotID, domain.RunTag(runID)) == nil {
		t.Fatalf("the recorded snapshot is not in the repository: %+v", snapshots)
	}

	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 || coverage[0].SnapshotID != backup.VerifiedSnapshotID {
		t.Fatalf("coverage does not name the snapshot that was written: %#v", coverage)
	}
}

// C. A second backup of changed staging produces a new snapshot and leaves the
// first readable.
func TestSpikeSecondBackupKeepsTheFirstSnapshot(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx := context.Background()

	firstRun, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	first := h.backupRun(firstRun)
	if first.Stage != domain.StageDurableSuccess {
		t.Fatalf("first backup did not succeed: %#v", first)
	}

	currentDir, err := h.layout.CurrentDir(source)
	if err != nil {
		t.Fatalf("current dir: %v", err)
	}
	writeFile(t, filepath.Join(currentDir, "document.txt"), "changed content, longer than before")
	writeFile(t, filepath.Join(currentDir, "added.txt"), "a new file")

	secondRun, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	second := h.backupRun(secondRun)
	if second.Stage != domain.StageDurableSuccess {
		t.Fatalf("second backup did not succeed: %#v", second)
	}
	if second.VerifiedSnapshotID == first.VerifiedSnapshotID {
		t.Fatal("the second backup reused the first snapshot")
	}

	inspector, spec := h.spikeInspector(t)
	snapshots, err := inspector.Snapshots(ctx, spec)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if findSnapshot(snapshots, first.VerifiedSnapshotID, domain.RunTag(firstRun)) == nil {
		t.Fatal("the first snapshot disappeared after a second backup")
	}
	if findSnapshot(snapshots, second.VerifiedSnapshotID, domain.RunTag(secondRun)) == nil {
		t.Fatal("the second snapshot is not in the repository")
	}
}

// D and J. A backup whose media could not be flushed and unmounted is not a
// success even though restic really wrote a snapshot, and a later explicit
// recovery finds that snapshot and adopts the run.
func TestSpikeInterruptedBackupIsRecoveredByAdoption(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	source := h.seedSource("documents", 2)
	batch := h.seedRevisionBatch(source, true)
	ctx := context.Background()

	// The snapshot is written for real; the durability boundary is what fails.
	h.media.unmountErr = domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorUnmountFailed,
		"the filesystem is still mounted at the ROTUP mount point after unmounting")

	runID, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup that was never proven durable was reported as a success")
	}
	interrupted := h.backupRun(runID)
	if interrupted.Stage == domain.StageDurableSuccess {
		t.Fatal("an unproven backup reached the durable success stage")
	}
	if interrupted.CandidateSnapshotID == "" && interrupted.VerifiedSnapshotID == "" {
		t.Fatal("no snapshot identifier was preserved for recovery")
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("an unproven backup recorded coverage")
	}

	// The media comes back and an administrator asks for a recovery.
	// A failing recovery flush must not adopt the existing real snapshot either.
	failedRecovery, err := h.service.StartRepositoryRecovery(ctx, repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.run(failedRecovery).Status.Successful() || h.run(runID).Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("failed recovery durability granted success or coverage")
	}
	h.media.unmountErr = nil
	recoveryRun, err := h.service.StartRepositoryRecovery(ctx, repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if recovery := h.run(recoveryRun); recovery.Status != domain.RunStatusSucceeded {
		t.Fatalf("recovery did not succeed: %s / %s", recovery.Status, recovery.SafeError)
	}

	adopted := h.run(runID)
	if !adopted.Status.Successful() {
		t.Fatalf("the interrupted run was not adopted: %s / %s", adopted.Status, adopted.SafeError)
	}
	if adopted.Status != domain.RunStatusSucceededWithWarning {
		t.Fatalf("an adopted run was recorded as an ordinary success: %s", adopted.Status)
	}
	adoptedBackup := h.backupRun(runID)
	if adoptedBackup.Durability != domain.DurabilityAdopted {
		t.Fatalf("durability = %s, want ADOPTED_AFTER_INTERRUPTION", adoptedBackup.Durability)
	}
	t.Logf("adopted snapshot %s after recovery", adoptedBackup.VerifiedSnapshotID)

	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 || coverage[0].SnapshotID != adoptedBackup.VerifiedSnapshotID {
		t.Fatalf("adopted coverage = %#v", coverage)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
		t.Fatalf("repository health after recovery = %s", health)
	}
}

// F. A repository whose identity does not match what ROTUP recorded is refused,
// against a real repository rather than a scripted one.
func TestSpikeWrongRepositoryIdentityIsRefused(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx := context.Background()

	// Replace the repository on the media with a different real one, exactly as
	// restoring the wrong disk image would.
	repositoryDir := filepath.Join(h.media.mountPoint, "rotup", "restic")
	if err := os.RemoveAll(repositoryDir); err != nil {
		t.Fatalf("remove repository: %v", err)
	}
	inspector, spec := h.spikeInspector(t)
	replacement, err := inspector.InitializeRepository(ctx, spec)
	if err != nil {
		t.Fatalf("create a replacement repository: %v", err)
	}
	if replacement.ID == repository.ResticRepositoryID {
		t.Fatal("the replacement repository has the same identity as the original")
	}

	runID, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup ran against a repository with a different identity")
	}
	if run.ErrorCode != domain.RepositoryErrorIdentityMismatch {
		t.Fatalf("error code = %q", run.ErrorCode)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}

	// And the replacement repository must still hold no ROTUP snapshot.
	snapshots, err := inspector.Snapshots(ctx, spec)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("a refused backup wrote %d snapshot(s)", len(snapshots))
	}
}

// I, as far as it can be modelled without hardware: the media disappearing
// during the operation. The Linux spike covers the real device path.
func TestSpikeSurpriseRemovalProducesNoSuccess(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	ctx := context.Background()

	// The repository directory is taken away mid-operation, and the disk stops
	// being observed, which is what a pulled cable looks like from here.
	h.media.vanishOnUnmount = true
	repositoryDir := filepath.Join(h.media.mountPoint, "rotup", "restic")
	if err := os.RemoveAll(repositoryDir); err != nil {
		t.Fatalf("remove repository: %v", err)
	}
	h.media.mu.Lock()
	h.media.present = false
	h.media.mu.Unlock()

	runID, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup against media that vanished was reported as a success")
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a backup against vanished media recorded coverage")
	}
	if h.backupRun(runID).Stage == domain.StageDurableSuccess {
		t.Fatal("a backup against vanished media reached the durable success stage")
	}
	t.Logf("vanished media produced %s / %s", run.Status, run.ErrorCode)
}

// G. A repository that recovery cannot prove never returns to healthy, using a
// real repository that has been damaged.
func TestSpikeUnreadableRepositoryIsNotRecovered(t *testing.T) {
	h := newSpikeHarness(t)
	repository := h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx := context.Background()

	if _, err := h.service.StartBackup(ctx, repository.ID, []string{source.ID}); err != nil {
		t.Fatalf("start backup: %v", err)
	}
	// Remove the repository configuration, so it can no longer be opened.
	if err := os.Remove(filepath.Join(h.media.mountPoint, "rotup", "restic", "config")); err != nil {
		t.Fatalf("damage the repository: %v", err)
	}

	runID, err := h.service.StartRepositoryRecovery(ctx, repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("recovery succeeded against a repository it could not open")
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}
}
