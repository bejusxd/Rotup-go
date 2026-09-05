package backup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
	"github.com/bejusxd/rotup/internal/staging"
)

const (
	testResticID   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSnapshotID = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testPassword   = "an-adequately-long-password"
	testDiskUUID   = "aaaaaaaa-1111-2222-3333-444444444444"
)

// harness wires the real store, the real staging layout, the real secret store
// and the real barrier around fake media and a fake engine.
//
// Using the real persistence is deliberate: the durability rules this milestone
// is about are partly enforced by database constraints, and a fake store would
// let a test pass against a schema that would reject the same write in
// production.
type harness struct {
	t       *testing.T
	store   *sqlite.Store
	service *Service
	// engine is the scripted engine. The integration spike replaces it with the
	// real restic adapter and leaves this nil.
	engine  *fakeEngine
	media   *fakeMedia
	layout  *staging.Layout
	barrier *staging.Barrier
	secrets *secrets.FileStore

	disk       domain.Disk
	repository domain.Repository
	now        time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
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
		engine:  newFakeEngine(testResticID),
		media:   newFakeMedia(t),
		layout:  layout,
		barrier: staging.NewBarrier(),
		secrets: secretStore,
		now:     time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC),
	}
	h.engine.backupResult = ports.BackupResult{Stats: ports.BackupStats{
		Available: true, SnapshotID: testSnapshotID,
		FilesNew: 3, FilesProcessed: 11, BytesProcessed: 4096, BytesAdded: 2048,
	}}

	h.service = New(ctx, Options{
		Store:     store,
		Engine:    h.engine,
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

// registerDisk creates the registered disk every repository operation needs.
func (h *harness) registerDisk() domain.Disk {
	h.t.Helper()
	id, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	disk := domain.Disk{
		ID: id, Name: "disk-a", FilesystemUUID: testDiskUUID, FilesystemType: "ext4",
		Enabled: true, FirstRegisteredAt: h.now, CurrentStatus: domain.DiskAvailableUnmounted,
		UpdatedAt: h.now,
	}
	if err := h.store.CreateDisk(context.Background(), disk); err != nil {
		h.t.Fatalf("create disk: %v", err)
	}
	h.disk = disk
	return disk
}

// establishRepository runs the real initialization path so tests start from a
// repository that was actually proven, not one inserted behind the service.
func (h *harness) establishRepository() domain.Repository {
	h.t.Helper()
	ctx := context.Background()
	if h.disk.ID == "" {
		h.registerDisk()
	}
	runID, err := h.service.StartRepositorySetup(ctx, SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize,
		Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		h.t.Fatalf("start repository setup: %v", err)
	}
	run, err := h.store.Run(ctx, runID)
	if err != nil || run == nil {
		h.t.Fatalf("load setup run: %v", err)
	}
	if run.Status != domain.RunStatusSucceeded {
		h.t.Fatalf("repository setup did not succeed: %s / %s", run.Status, run.SafeError)
	}
	repository, err := h.store.RepositoryForDisk(ctx, h.disk.ID)
	if err != nil || repository == nil {
		h.t.Fatalf("load repository: %v", err)
	}
	h.repository = *repository
	return *repository
}

// seedSource creates a source with a published stable staging state and real
// files under the staging root, so the read-set has something to resolve.
func (h *harness) seedSource(name string, version int64) domain.Source {
	h.t.Helper()
	ctx := context.Background()
	id, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	published := h.now
	source := domain.Source{
		ID: id, Name: name, Host: "fileserver.example", ShareName: "docs",
		StagingRelativePath: name, Enabled: true,
		ReconciliationPolicy: domain.PolicyArchiveThenSync,
		StagingStatus:        domain.StagingStable, StagingStateVersion: version,
		LastPublishedAt: &published,
	}
	if err := h.store.CreateSource(ctx, source); err != nil {
		h.t.Fatalf("create source: %v", err)
	}

	collectionRunID, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	if err := h.store.RecordRun(ctx, domain.Run{
		ID: collectionRunID, Kind: domain.RunKindCollection, SourceID: id,
		Status: domain.RunStatusSucceeded, RequestedAt: h.now, UpdatedAt: h.now,
	}); err != nil {
		h.t.Fatalf("record collection run: %v", err)
	}
	source.LastCollectionRunID = collectionRunID
	if err := h.store.SaveSourceResult(ctx, domain.SourceResult{
		RunID: collectionRunID, SourceID: id, Status: domain.RunStatusSucceeded,
		StagingStateVersion: version, PublishedAt: published,
	}, &source); err != nil {
		h.t.Fatalf("save source result: %v", err)
	}

	paths, err := h.layout.PreparePaths(source, collectionRunID, true)
	if err != nil {
		h.t.Fatalf("prepare staging paths: %v", err)
	}
	writeFile(h.t, filepath.Join(paths.Current, "document.txt"), "hello")
	return source
}

// seedRevisionBatch adds a completed revision batch with real files.
func (h *harness) seedRevisionBatch(source domain.Source, completed bool) domain.RevisionBatch {
	h.t.Helper()
	ctx := context.Background()
	runID, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	if err := h.store.RecordRun(ctx, domain.Run{
		ID: runID, Kind: domain.RunKindCollection, SourceID: source.ID,
		Status: domain.RunStatusSucceeded, RequestedAt: h.now, UpdatedAt: h.now,
	}); err != nil {
		h.t.Fatalf("record collection run: %v", err)
	}
	directory, err := h.layout.RevisionDir(source.ID, runID)
	if err != nil {
		h.t.Fatalf("revision dir: %v", err)
	}
	writeFile(h.t, filepath.Join(directory, "displaced.txt"), "old")

	batchID, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	batch := domain.RevisionBatch{
		ID: batchID, SourceID: source.ID, CollectionRunID: runID,
		RelativePath: staging.RevisionRelativePath(source.ID, runID),
		CreatedAt:    h.now, ItemCount: 1, ByteCount: 3,
		ReconciliationCompleted: completed,
	}
	if err := h.store.RecordRevisionBatch(ctx, batch); err != nil {
		h.t.Fatalf("record revision batch: %v", err)
	}
	return batch
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func (h *harness) run(runID string) domain.Run {
	h.t.Helper()
	run, err := h.store.Run(context.Background(), runID)
	if err != nil || run == nil {
		h.t.Fatalf("load run %s: %v", runID, err)
	}
	return *run
}

func (h *harness) backupRun(runID string) domain.BackupRun {
	h.t.Helper()
	backup, err := h.store.BackupRun(context.Background(), runID)
	if err != nil || backup == nil {
		h.t.Fatalf("load backup run %s: %v", runID, err)
	}
	return *backup
}

func (h *harness) repositoryNow() domain.Repository {
	h.t.Helper()
	repository, err := h.store.Repository(context.Background(), h.repository.ID)
	if err != nil || repository == nil {
		h.t.Fatalf("load repository: %v", err)
	}
	return *repository
}

func (h *harness) coverage(batchID string) []domain.RevisionCoverage {
	h.t.Helper()
	coverage, err := h.store.RevisionCoverageForBatch(context.Background(), batchID)
	if err != nil {
		h.t.Fatalf("load coverage: %v", err)
	}
	return coverage
}

// ---------------------------------------------------------------- repository

func TestRepositoryInitializationEstablishesAndProves(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	repository := h.establishRepository()

	if repository.Health != domain.RepositoryHealthy {
		t.Fatalf("repository health = %s", repository.Health)
	}
	if repository.ResticRepositoryID != testResticID {
		t.Fatalf("repository identity = %q", repository.ResticRepositoryID)
	}
	if repository.Origin != domain.RepositoryInitialized {
		t.Fatalf("repository origin = %s", repository.Origin)
	}
	if repository.EscrowAcknowledgedAt == nil {
		t.Fatal("the escrow acknowledgement was not recorded")
	}
	if repository.RelativePath != domain.RepositoryRelativePath {
		t.Fatalf("repository path = %q; it must be ROTUP's own constant", repository.RelativePath)
	}
	// Creating the repository is not enough on its own: the snapshot listing is
	// read too, which is what proves the repository can actually be opened with
	// the stored password rather than merely that a directory was written.
	// (The identity read-back itself happens inside the engine adapter, which
	// has its own test for refusing an unreadable configuration.)
	if !h.engine.called("init") || !h.engine.called("snapshots") {
		t.Fatalf("engine calls = %v", h.engine.calls)
	}
	if h.media.unmounts == 0 {
		t.Fatal("the disk was not unmounted after establishing the repository")
	}
	if h.media.busy(h.disk.ID) {
		t.Fatal("the disk lease was not released")
	}
}

func TestRepositoryInitializationRequiresEscrowAcknowledgement(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	_, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: testPassword,
		EscrowAcknowledged: false,
	})
	if err == nil {
		t.Fatal("a repository was established without an escrow acknowledgement")
	}
	if code := codeOf(t, err); code != domain.RepositoryErrorEscrowNotAcknowledged {
		t.Fatalf("error code = %q", code)
	}
	if h.engine.called("init") {
		t.Fatal("restic was invoked despite the refusal")
	}
}

func TestRepositoryInitializationRejectsWeakPassword(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	_, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: "short",
		EscrowAcknowledged: true,
	})
	if err == nil {
		t.Fatal("a trivially short repository password was accepted")
	}
}

// A repository is never written over a location ROTUP did not create.
func TestRepositoryInitializationRefusesOccupiedLocation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	writeFile(t, filepath.Join(h.media.mountPoint, "rotup", "restic", "config"), "someone else")

	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatalf("start setup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a repository was initialized over an occupied location")
	}
	if run.ErrorCode != domain.RepositoryErrorPathOccupied {
		t.Fatalf("error code = %q", run.ErrorCode)
	}
	if h.engine.called("init") {
		t.Fatal("restic init ran against an occupied location")
	}
}

// Adoption is only offered for a repository that is already there. Adopting an
// empty location would record a repository that does not exist.
func TestRepositoryAdoptionRefusesEmptyLocation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupAdopt, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatalf("start adoption: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("an empty location was adopted as a repository")
	}
	if run.ErrorCode != domain.RepositoryErrorNotEstablished {
		t.Fatalf("error code = %q", run.ErrorCode)
	}
}

func TestRepositoryAdoptionProvesAnExistingRepository(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	writeFile(t, filepath.Join(h.media.mountPoint, "rotup", "restic", "config"), "existing repository")

	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupAdopt, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatalf("start adoption: %v", err)
	}
	if run := h.run(runID); run.Status != domain.RunStatusSucceeded {
		t.Fatalf("adoption did not succeed: %s / %s", run.Status, run.SafeError)
	}
	repository, _ := h.store.RepositoryForDisk(context.Background(), h.disk.ID)
	if repository.Origin != domain.RepositoryAdopted {
		t.Fatalf("repository origin = %s", repository.Origin)
	}
	if repository.ResticRepositoryID != testResticID {
		t.Fatalf("adopted repository identity = %q", repository.ResticRepositoryID)
	}
	if h.engine.called("init") {
		t.Fatal("adoption created a repository instead of opening one")
	}
}

// A wrong password is an authentication failure at the engine, and must leave
// the repository unestablished rather than half-recorded.
func TestRepositoryAdoptionWithWrongPasswordFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	writeFile(t, filepath.Join(h.media.mountPoint, "rotup", "restic", "config"), "existing repository")
	h.engine.inspectErr = domain.NewError(domain.ErrClassAuthentication, "restic_wrong_password",
		"the stored repository password is not the right one")

	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupAdopt, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatalf("start adoption: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("adoption succeeded with the wrong password")
	}
	repository, _ := h.store.RepositoryForDisk(context.Background(), h.disk.ID)
	if repository.Established() {
		t.Fatal("a repository that could not be opened was recorded as established")
	}
}

// An established repository is never re-established: initializing would destroy
// its snapshots and adopting would replace the password that decrypts them.
func TestEstablishedRepositoryIsNotReEstablished(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()

	for _, mode := range []SetupMode{SetupInitialize, SetupAdopt} {
		_, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
			DiskID: h.disk.ID, Mode: mode, Password: testPassword, EscrowAcknowledged: true,
		})
		if err == nil {
			t.Fatalf("%s was allowed on an established repository", mode)
		}
		if code := codeOf(t, err); code != "repository_already_established" {
			t.Fatalf("%s error code = %q", mode, code)
		}
	}
}

// A repository whose disk was flushed but not unmounted is not proven to be on
// the media.
func TestRepositoryInitializationUnmountFailureIsSuspect(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registerDisk()
	h.media.unmountErr = domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorUnmountFailed,
		"the filesystem is still mounted at the ROTUP mount point after unmounting")

	runID, err := h.service.StartRepositorySetup(context.Background(), SetupInput{
		DiskID: h.disk.ID, Mode: SetupInitialize, Password: testPassword, EscrowAcknowledged: true,
	})
	if err != nil {
		t.Fatalf("start setup: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("a repository that could not be unmounted was reported as ready")
	}
	repository, _ := h.store.RepositoryForDisk(context.Background(), h.disk.ID)
	if repository.Health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", repository.Health)
	}
}

// The password must reach the secret store, and nothing but its locator may
// reach SQLite.
func TestRepositoryPasswordNeverReachesTheDatabase(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	ctx := context.Background()

	ref, err := h.store.RepositoryCredentialRef(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if ref.ProviderLocator == "" {
		t.Fatal("no repository credential was recorded")
	}
	if strings.Contains(ref.ProviderLocator, testPassword) || strings.Contains(ref.DisplayName, testPassword) {
		t.Fatal("the repository password appeared in credential metadata")
	}

	// The material has to actually be retrievable, or the repository could
	// never be opened again unattended.
	var seen string
	if err := h.secrets.Materialize(ctx, ref.ProviderLocator, func(material domain.SecretMaterial) error {
		seen = material.Reveal()
		return nil
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if seen != testPassword {
		t.Fatal("the stored repository password does not round-trip")
	}

	// And nowhere in the database itself.
	assertPasswordAbsentFromDatabase(t, h)
}

func assertPasswordAbsentFromDatabase(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	events, err := h.store.ListEvents(ctx, 500)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.PayloadJSON, testPassword) {
			t.Fatalf("the repository password appeared in event %s", event.EventType)
		}
	}
	runs, err := h.store.ListRuns(ctx, 500)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if strings.Contains(run.Summary, testPassword) ||
			strings.Contains(run.SummaryJSON, testPassword) ||
			strings.Contains(run.SafeError, testPassword) {
			t.Fatalf("the repository password appeared in run %s", run.ID)
		}
	}
}

// ------------------------------------------------------------------- backup

func TestBackupReachesDurableSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 3)
	batch := h.seedRevisionBatch(source, true)

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}

	run := h.run(runID)
	if !run.Status.Successful() {
		t.Fatalf("backup did not succeed: %s / %s", run.Status, run.SafeError)
	}
	backup := h.backupRun(runID)
	if backup.Stage != domain.StageDurableSuccess {
		t.Fatalf("durability stage = %s", backup.Stage)
	}
	if backup.Durability != domain.DurabilityNormal {
		t.Fatalf("durability = %s", backup.Durability)
	}
	if backup.VerifiedSnapshotID != testSnapshotID {
		t.Fatalf("verified snapshot = %q", backup.VerifiedSnapshotID)
	}
	if backup.FilesProcessed != 11 || backup.BytesProcessed != 4096 || backup.BytesAdded != 2048 {
		t.Fatalf("engine counters were not recorded: %#v", backup)
	}

	// The snapshot has to have been confirmed by a separate read, not just
	// reported by the backup command.
	if !h.engine.called("snapshots") {
		t.Fatal("the snapshot was never independently verified")
	}
	// And the media must be flushed and unmounted before success.
	if h.media.unmounts == 0 {
		t.Fatal("the disk was not unmounted")
	}
	if h.media.busy(h.disk.ID) {
		t.Fatal("the disk lease was not released")
	}

	// Coverage names exactly the batch that was in the read-set, and says which
	// physical copy holds it.
	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 {
		t.Fatalf("coverage rows = %d, want 1", len(coverage))
	}
	if coverage[0].SnapshotID != testSnapshotID || coverage[0].DiskID != h.disk.ID ||
		coverage[0].RepositoryID != h.repository.ID || coverage[0].BackupRunID != runID {
		t.Fatalf("coverage does not identify the copy: %#v", coverage[0])
	}

	repository := h.repositoryNow()
	if repository.LastSnapshotID != testSnapshotID || repository.LastSuccessfulBackupAt == nil {
		t.Fatalf("the repository did not record the backup: %#v", repository)
	}
	assertPasswordAbsentFromDatabase(t, h)
}

// The snapshot must carry this run's tag; a repository that merely contains a
// snapshot with a matching identifier proves nothing about this run.
func TestBackupTagsSnapshotWithRunIdentity(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	wanted := domain.RunTag(runID)
	found := false
	for _, tag := range h.engine.backupTags {
		if tag == wanted {
			found = true
		}
		if strings.Contains(tag, testPassword) {
			t.Fatal("a snapshot tag contained the repository password")
		}
	}
	if !found {
		t.Fatalf("snapshot tags = %v, want one containing the run identity", h.engine.backupTags)
	}
}

// The backup must read exactly the captured paths and nothing else: no work
// area, no lost+found, no secret material.
func TestBackupReadsOnlyTheCapturedPaths(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	h.seedRevisionBatch(source, true)

	if _, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID}); err != nil {
		t.Fatalf("start backup: %v", err)
	}

	for _, path := range h.engine.backupPaths {
		normalized := filepath.ToSlash(path)
		for _, forbidden := range []string{"/work/", "/lost+found", "secrets"} {
			if strings.Contains(normalized, forbidden) {
				t.Fatalf("the backup read %s, which must never be included", path)
			}
		}
		if !filepath.IsAbs(path) {
			t.Fatalf("the backup was given a relative path %q", path)
		}
	}
	if len(h.engine.backupPaths) == 0 {
		t.Fatal("the backup was given no paths at all")
	}
}

// An incomplete revision batch is not a complete group of displaced objects, so
// it must not be backed up or covered.
func TestBackupExcludesIncompleteRevisionBatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	complete := h.seedRevisionBatch(source, true)
	incomplete := h.seedRevisionBatch(source, false)

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}

	captured, err := h.store.BackupRevisionBatches(context.Background(), runID)
	if err != nil {
		t.Fatalf("load read-set batches: %v", err)
	}
	if len(captured) != 1 || captured[0].RevisionBatchID != complete.ID {
		t.Fatalf("captured batches = %v, want only the completed one", captured)
	}
	if len(h.coverage(incomplete.ID)) != 0 {
		t.Fatal("an incomplete revision batch was recorded as covered")
	}
}

// The read-set is a record of what was read, and it is written before restic
// starts so a crash cannot leave a snapshot nobody can attribute.
func TestReadSetCapturesExactStateAndIsImmutable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 5)
	other := h.seedSource("photos", 2)
	batch := h.seedRevisionBatch(source, true)

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	ctx := context.Background()

	states, err := h.store.BackupSourceStates(ctx, runID)
	if err != nil {
		t.Fatalf("load source states: %v", err)
	}
	if len(states) != 1 || states[0].SourceID != source.ID {
		t.Fatalf("read-set sources = %v, want only the selected one", states)
	}
	if states[0].StagingStateVersion != 5 {
		t.Fatalf("captured staging version = %d, want 5", states[0].StagingStateVersion)
	}
	if states[0].CurrentCollectionRunID == "" {
		t.Fatal("the read-set does not name the collection that published the state")
	}
	if !strings.Contains(states[0].CurrentRelativePath, "documents") {
		t.Fatalf("captured path = %q", states[0].CurrentRelativePath)
	}
	if states[0].CapturedAt.IsZero() {
		t.Fatal("the read-set has no capture time")
	}
	batches, _ := h.store.BackupRevisionBatches(ctx, runID)
	if len(batches) != 1 || batches[0].RevisionBatchID != batch.ID {
		t.Fatalf("read-set batches = %v", batches)
	}

	// The unselected source must be nowhere in the read-set.
	for _, state := range states {
		if state.SourceID == other.ID {
			t.Fatal("an unselected source appeared in the read-set")
		}
	}

	// Changing configuration afterwards must not rewrite what the run recorded.
	renamed := source
	renamed.Name = "renamed"
	renamed.Enabled = false
	renamed.StagingStateVersion = 99
	if err := h.store.UpdateSourceSettings(ctx, renamed); err != nil {
		t.Fatalf("update source: %v", err)
	}
	statesAfter, _ := h.store.BackupSourceStates(ctx, runID)
	if len(statesAfter) != 1 || statesAfter[0].StagingStateVersion != 5 {
		t.Fatalf("the recorded read-set changed with configuration: %v", statesAfter)
	}
	backup := h.backupRun(runID)
	if len(backup.SelectedSourceIDs) != 1 || backup.SelectedSourceIDs[0] != source.ID {
		t.Fatalf("the run target changed with configuration: %v", backup.SelectedSourceIDs)
	}
	if backup.DiskID != h.disk.ID || backup.RepositoryID != h.repository.ID {
		t.Fatal("the run target no longer names the disk and repository it was created for")
	}
}

func TestBackupRefusesIneligibleSource(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		status domain.StagingStatus
		code   string
	}{
		{"updating", domain.StagingUpdating, "source_updating"},
		{"recovery required", domain.StagingRecoveryRequired, "source_recovery_required"},
		{"never collected", domain.StagingEmpty, "source_never_collected"},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.establishRepository()
			source := h.seedSource("documents", 1)
			if err := h.store.SetSourceStagingStatus(context.Background(), source.ID, testCase.status); err != nil {
				t.Fatalf("set staging status: %v", err)
			}

			runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
			if err != nil {
				t.Fatalf("start backup: %v", err)
			}
			run := h.run(runID)
			if run.Status.Successful() {
				t.Fatalf("a %s source was backed up", testCase.status)
			}
			if run.ErrorCode != testCase.code {
				t.Fatalf("error code = %q, want %q", run.ErrorCode, testCase.code)
			}
			if h.engine.called("backup") {
				t.Fatal("restic ran despite an ineligible source")
			}
			// Nothing was written, so the repository keeps its health.
			if h.repositoryNow().Health != domain.RepositoryHealthy {
				t.Fatal("the repository was marked suspect by a failure that touched nothing")
			}
		})
	}
}

// Prefer all-or-nothing: one unsafe source fails the whole run rather than
// producing a partial snapshot presented as a success.
func TestBackupFailsWhenAnySelectedSourceIsUnsafe(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	good := h.seedSource("documents", 1)
	bad := h.seedSource("photos", 1)
	if err := h.store.SetSourceStagingStatus(context.Background(), bad.ID,
		domain.StagingRecoveryRequired); err != nil {
		t.Fatalf("set staging status: %v", err)
	}

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID,
		[]string{good.ID, bad.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("a backup with one unsafe source succeeded")
	}
	if h.engine.called("backup") {
		t.Fatal("restic ran with an unsafe source in the selection")
	}
}

func TestBackupRefusesUnprovenRepository(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	if err := h.store.SetRepositoryHealth(context.Background(), h.repository.ID,
		domain.RepositorySuspect, "an interrupted backup"); err != nil {
		t.Fatalf("set health: %v", err)
	}

	_, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err == nil {
		t.Fatal("a backup was allowed against a suspect repository")
	}
	if code := codeOf(t, err); code != "repository_not_writable" {
		t.Fatalf("error code = %q", code)
	}
}

// The repository identity is verified after mounting and before any write.
func TestBackupRefusesRepositoryIdentityMismatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	h.engine.repositoryID = "1111111111111111111111111111111111111111111111111111111111111111"

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
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
	if h.engine.called("backup") {
		t.Fatal("restic wrote to a repository whose identity did not match")
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}
}

func TestBackupEngineFailureLeavesRepositorySuspect(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	h.engine.backupErr = domain.NewError(domain.ErrClassExternalTool, "restic_command_failed",
		"the backup failed")

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("a failed restic run was reported as a success")
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
	backup := h.backupRun(runID)
	// The stage is preserved so a later recovery knows restic had started.
	if backup.Stage != domain.StageResticRunning {
		t.Fatalf("stage = %s, want the stage that says restic had started", backup.Stage)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a failed backup recorded revision coverage")
	}
}

// restic naming a snapshot that is not in the repository must never be a
// success. This is the case the independent verification exists for.
func TestBackupFailsWhenSnapshotCannotBeVerified(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	h.engine.recordSnapshot = false

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a snapshot that is not in the repository was reported as a success")
	}
	if run.ErrorCode != domain.RepositoryErrorSnapshotAbsent {
		t.Fatalf("error code = %q", run.ErrorCode)
	}
	backup := h.backupRun(runID)
	if backup.CandidateSnapshotID != testSnapshotID {
		t.Fatal("the candidate snapshot was not preserved for recovery")
	}
	if backup.VerifiedSnapshotID != "" {
		t.Fatal("an unverified snapshot was recorded as verified")
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("an unverified backup recorded revision coverage")
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
}

// A verified snapshot is still not a durable success until the media has been
// flushed and the unmount confirmed.
func TestBackupWithFailedUnmountIsNotASuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	h.media.unmountErr = domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorUnmountFailed,
		"the filesystem is still mounted at the ROTUP mount point after unmounting")

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup whose disk could not be unmounted was reported as a success")
	}
	backup := h.backupRun(runID)
	if backup.Stage == domain.StageDurableSuccess {
		t.Fatal("an unproven backup reached the durable success stage")
	}
	// The flush completed, so that fact is kept even though the unmount failed.
	if backup.Stage != domain.StageSynced {
		t.Fatalf("stage = %s, want SYNCED", backup.Stage)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a backup that was never proven durable recorded coverage")
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
}

func TestBackupWithFailedSyncIsNotASuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	h.media.unmountErr = domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorSyncFailed,
		"the filesystem could not be flushed")

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("a backup whose disk could not be flushed was reported as a success")
	}
	backup := h.backupRun(runID)
	if backup.Stage != domain.StageSnapshotVerified {
		t.Fatalf("stage = %s, want SNAPSHOT_VERIFIED for a failed flush", backup.Stage)
	}
}

// The disk disappearing mid-backup must produce an honest interruption, never a
// success and never a generic error that hides what happened.
func TestSurpriseRemovalDuringBackupIsNotASuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)

	h.engine.backupErr = domain.NewError(domain.ErrClassIntegrity, "restic_media_error",
		"the backup failed with a filesystem error from the media")
	h.engine.onBackup = func() {
		h.media.mu.Lock()
		h.media.present = false
		h.media.mu.Unlock()
	}

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup interrupted by a surprise removal was reported as a success")
	}
	if run.ErrorCode != domain.DiskErrorRemovedDuringOperation {
		t.Fatalf("error code = %q, want the removal code", run.ErrorCode)
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("an interrupted backup recorded coverage")
	}
	if h.media.busy(h.disk.ID) {
		t.Fatal("the disk lease was not released after the interruption")
	}
}

// A second manual backup of the same disk is refused cleanly rather than
// queued or run concurrently.
func TestDuplicateBackupRequestIsRefusedCleanly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)

	// Hold the lease as another operation would.
	lease, err := h.media.Acquire(context.Background(), h.disk.ID)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer lease.Release()

	_, err = h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err == nil {
		t.Fatal("a second backup of a busy disk was accepted")
	}
	if class := domain.ClassOf(err); class != domain.ErrClassUnavailable {
		t.Fatalf("error class = %s, want UNAVAILABLE", class)
	}
	// A refused request must not leave an orphaned run behind.
	runs, err := h.store.ListRuns(context.Background(), 100)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, run := range runs {
		if run.Kind == domain.RunKindBackupLatest {
			t.Fatal("a refused backup request created a run row")
		}
	}
}

// ------------------------------------------------------------ staging barrier

func TestBackupBlocksCollectionMutationWhileItReads(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	other := h.seedSource("photos", 1)

	var duringBackup error
	var unrelated error
	h.engine.onBackup = func() {
		_, duringBackup = h.barrier.AcquireMutation(source.ID)
		var release func()
		release, unrelated = h.barrier.AcquireMutation(other.ID)
		if release != nil {
			release()
		}
	}

	if _, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID}); err != nil {
		t.Fatalf("start backup: %v", err)
	}

	if duringBackup == nil {
		t.Fatal("a collection was allowed to reconcile a source while the backup was reading it")
	}
	if unrelated == nil {
		t.Fatal("the daemon-wide staging mutation barrier allowed collection during backup")
	}
	// The barrier must be released once the run ends, whatever happened.
	if h.barrier.BeingRead(source.ID) {
		t.Fatal("the staging barrier was not released after the backup")
	}
}

func TestBarrierIsReleasedAfterAFailedBackup(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	h.engine.backupErr = errEngineFailed

	if _, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID}); err != nil {
		t.Fatalf("start backup: %v", err)
	}
	if h.barrier.BeingRead(source.ID) {
		t.Fatal("the staging barrier survived a failed backup")
	}
	release, err := h.barrier.AcquireMutation(source.ID)
	if err != nil {
		t.Fatalf("collection is still blocked after a failed backup: %v", err)
	}
	release()
}

func TestBackupIsRefusedWhileCollectionMutates(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)

	release, err := h.barrier.AcquireMutation(source.ID)
	if err != nil {
		t.Fatalf("acquire mutation: %v", err)
	}
	defer release()

	runID, err := h.service.StartBackup(context.Background(), h.repository.ID, []string{source.ID})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("a backup read a source that a collection was reconciling")
	}
	if h.engine.called("backup") {
		t.Fatal("restic ran while a collection was mutating the read-set")
	}
	// Nothing was read or written, so the repository is untouched.
	if h.repositoryNow().Health != domain.RepositoryHealthy {
		t.Fatal("a backup refused before it read anything marked the repository suspect")
	}
}

// -------------------------------------------------------------------- recovery

// A run a previous process left active is never resolved optimistically.
func TestStartupRecoveryInterruptsAndSuspects(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx := context.Background()

	// A run that got as far as restic.
	late := h.startStuckBackup(source, domain.StageResticRunning, testSnapshotID)
	// A run that never started restic.
	early := h.startStuckBackup(source, domain.StageReadSetCaptured, "")

	report, err := h.service.Recover(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.InterruptedRuns != 2 {
		t.Fatalf("interrupted runs = %d, want 2", report.InterruptedRuns)
	}
	if report.RepositoriesSuspected != 1 {
		t.Fatalf("repositories suspected = %d, want 1", report.RepositoriesSuspected)
	}

	for _, runID := range []string{late, early} {
		run := h.run(runID)
		if run.Status != domain.RunStatusInterrupted {
			t.Fatalf("run %s status = %s, want INTERRUPTED", runID, run.Status)
		}
		if run.Status.Successful() {
			t.Fatal("a stale run became a success")
		}
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
	// The run that never started restic must not be presented as recoverable.
	if h.backupRun(early).CandidateForRecovery() {
		t.Fatal("a run that never started restic was marked as needing recovery")
	}
	if !h.backupRun(late).CandidateForRecovery() {
		t.Fatal("a run that started restic was not marked as needing recovery")
	}
}

// startStuckBackup creates a backup run left in a given stage, as a crash would.
func (h *harness) startStuckBackup(source domain.Source, stage domain.DurabilityStage,
	candidate string) string {

	h.t.Helper()
	ctx := context.Background()
	runID, err := domain.NewID()
	if err != nil {
		h.t.Fatalf("new id: %v", err)
	}
	if err := h.store.RecordRun(ctx, domain.Run{
		ID: runID, Kind: domain.RunKindBackupLatest, TriggerKind: domain.RunTriggerManual,
		Status: domain.RunStatusRunning, RequestedAt: h.now, StartedAt: h.now, UpdatedAt: h.now,
	}); err != nil {
		h.t.Fatalf("record run: %v", err)
	}
	if err := h.store.CreateBackupRun(ctx, domain.BackupRun{
		RunID: runID, RepositoryID: h.repository.ID, DiskID: h.disk.ID,
		Stage: domain.StageRequested, SelectedSourceIDs: []string{source.ID},
	}); err != nil {
		h.t.Fatalf("create backup run: %v", err)
	}
	relative, err := staging.CurrentRelativePath(source)
	if err != nil {
		h.t.Fatalf("current relative path: %v", err)
	}
	if err := h.store.SaveReadSet(ctx, runID, []domain.BackupSourceState{{
		BackupRunID: runID, SourceID: source.ID,
		CurrentCollectionRunID: source.LastCollectionRunID,
		StagingStateVersion:    source.StagingStateVersion,
		PublishedAt:            h.now, CurrentRelativePath: relative, CapturedAt: h.now,
	}}, nil); err != nil {
		h.t.Fatalf("save read-set: %v", err)
	}
	if err := h.store.SetBackupStage(ctx, runID, stage, h.now); err != nil {
		h.t.Fatalf("set stage: %v", err)
	}
	if candidate != "" {
		if err := h.store.RecordCandidateSnapshot(ctx, runID, candidate, domain.BackupRun{}); err != nil {
			h.t.Fatalf("record candidate: %v", err)
		}
		if err := h.store.SetBackupStage(ctx, runID, stage, h.now); err != nil {
			h.t.Fatalf("restore stage: %v", err)
		}
	}
	return runID
}

// Recovery adopts an interrupted backup only after finding its snapshot, and
// records the result as adopted rather than as an ordinary success.
func TestRecoveryAdoptsAProvenSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 4)
	batch := h.seedRevisionBatch(source, true)
	ctx := context.Background()

	interrupted := h.startStuckBackup(source, domain.StageSnapshotCreated, testSnapshotID)
	// The batch was in the read-set of the interrupted run.
	if err := h.store.SaveReadSet(ctx, interrupted, nil, []domain.BackupRevisionBatch{{
		BackupRunID: interrupted, RevisionBatchID: batch.ID, SourceID: source.ID,
		RelativePath: batch.RelativePath, BatchCreatedAt: batch.CreatedAt, CapturedAt: h.now,
	}}); err != nil {
		t.Fatalf("save read-set batches: %v", err)
	}
	if _, err := h.service.Recover(ctx); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}

	// The snapshot really is in the repository, carrying the run's tag.
	h.engine.snapshots = []ports.Snapshot{{
		ID: testSnapshotID, ShortID: shortID(testSnapshotID),
		Tags: []string{domain.TagROTUP, domain.RunTag(interrupted)},
		Paths: []string{
			filepath.Join(h.layout.Root(), "sources", source.StagingRelativePath, "current"),
			filepath.Join(h.layout.Root(), filepath.FromSlash(batch.RelativePath)),
		},
	}}

	runID, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if run := h.run(runID); run.Status != domain.RunStatusSucceeded {
		t.Fatalf("recovery did not succeed: %s / %s", run.Status, run.SafeError)
	}
	if !h.engine.called("check") {
		t.Fatal("recovery did not run a structural repository check")
	}

	adopted := h.run(interrupted)
	if !adopted.Status.Successful() {
		t.Fatalf("the interrupted run was not adopted: %s", adopted.Status)
	}
	// It is a success, but deliberately not presented as the normal one.
	if adopted.Status != domain.RunStatusSucceededWithWarning {
		t.Fatalf("adopted run status = %s, want SUCCEEDED_WITH_WARNING", adopted.Status)
	}
	if !strings.Contains(adopted.Summary, "recovered") {
		t.Fatalf("adopted run summary does not say it was recovered: %q", adopted.Summary)
	}
	backup := h.backupRun(interrupted)
	if backup.Durability != domain.DurabilityAdopted {
		t.Fatalf("durability = %s, want ADOPTED_AFTER_INTERRUPTION", backup.Durability)
	}
	if backup.Stage != domain.StageDurableSuccess || backup.VerifiedSnapshotID != testSnapshotID {
		t.Fatalf("adopted backup = %#v", backup)
	}

	// Coverage is exactly the read-set the interrupted run had recorded.
	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 || coverage[0].BackupRunID != interrupted ||
		coverage[0].SnapshotID != testSnapshotID || coverage[0].DiskID != h.disk.ID {
		t.Fatalf("adopted coverage = %#v", coverage)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
		t.Fatalf("repository health after recovery = %s", health)
	}
}

// A candidate snapshot that is not in the repository is not adopted, and its
// read-set is not covered.
func TestRecoveryDoesNotAdoptAnAbsentSnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	batch := h.seedRevisionBatch(source, true)
	ctx := context.Background()

	interrupted := h.startStuckBackup(source, domain.StageSnapshotCreated, testSnapshotID)
	if err := h.store.SaveReadSet(ctx, interrupted, nil, []domain.BackupRevisionBatch{{
		BackupRunID: interrupted, RevisionBatchID: batch.ID, SourceID: source.ID,
		RelativePath: batch.RelativePath, BatchCreatedAt: batch.CreatedAt, CapturedAt: h.now,
	}}); err != nil {
		t.Fatalf("save read-set batches: %v", err)
	}
	if _, err := h.service.Recover(ctx); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}

	// The repository holds no snapshot for that run.
	h.engine.snapshots = nil

	runID, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if run := h.run(runID); run.Status != domain.RunStatusSucceeded {
		t.Fatalf("recovery did not complete: %s / %s", run.Status, run.SafeError)
	}

	abandoned := h.run(interrupted)
	if abandoned.Status.Successful() {
		t.Fatal("a run with no snapshot in the repository was adopted as a success")
	}
	if abandoned.ErrorCode != domain.RepositoryErrorSnapshotAbsent {
		t.Fatalf("abandoned run error code = %q", abandoned.ErrorCode)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("an abandoned run recorded revision coverage")
	}
	// The repository itself was proven, so it is usable again.
	if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
		t.Fatalf("repository health = %s", health)
	}
}

// A repository whose identity does not match is never returned to healthy.
func TestRecoveryRefusesIdentityMismatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	h.engine.repositoryID = "2222222222222222222222222222222222222222222222222222222222222222"

	runID, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	run := h.run(runID)
	if run.Status.Successful() {
		t.Fatal("recovery succeeded against a repository with a different identity")
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}
}

// A failing structural check must not return a repository to healthy.
func TestRecoveryRefusesFailedCheck(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	h.engine.checkErr = domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorUnreadable,
		"the repository check reported 3 problem(s)")

	runID, err := h.service.StartRepositoryRecovery(context.Background(), h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if run := h.run(runID); run.Status.Successful() {
		t.Fatal("recovery succeeded despite a failing repository check")
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}
}

// A repository another live process holds must be left alone, and must be
// recoverable once that process is gone.
//
// The dangerous shape here is not the refusal itself but what surrounds it: the
// interrupted backup in this test is genuinely adoptable — its snapshot exists,
// carries the run's tag and matches the recorded read-set — so everything except
// the lock is in place for recovery to grant coverage. A recovery that treated
// "cannot open the repository right now" as anything other than a hard stop
// would therefore adopt a backup it never actually proved.
//
// ROTUP has no way to see a lock directly: it runs restic's `check` and
// `snapshots` without --no-lock and believes the refusal. So the lock is
// injected exactly where a real one surfaces, as the engine's answer, and
// clearing it models the owning process exiting. Nothing here unlocks anything:
// ROTUP has no unlock path, which is the point.
func TestRecoveryRefusesALockedRepositoryAndResumesWhenItIsReleased(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 4)
	batch := h.seedRevisionBatch(source, true)
	ctx := context.Background()

	interrupted := h.startStuckBackup(source, domain.StageSnapshotCreated, testSnapshotID)
	if err := h.store.SaveReadSet(ctx, interrupted, nil, []domain.BackupRevisionBatch{{
		BackupRunID: interrupted, RevisionBatchID: batch.ID, SourceID: source.ID,
		RelativePath: batch.RelativePath, BatchCreatedAt: batch.CreatedAt, CapturedAt: h.now,
	}}); err != nil {
		t.Fatalf("save read-set batches: %v", err)
	}
	if _, err := h.service.Recover(ctx); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	h.engine.snapshots = []ports.Snapshot{{
		ID: testSnapshotID, ShortID: shortID(testSnapshotID),
		Tags: []string{domain.TagROTUP, domain.RunTag(interrupted)},
		Paths: []string{
			filepath.Join(h.layout.Root(), "sources", source.StagingRelativePath, "current"),
			filepath.Join(h.layout.Root(), filepath.FromSlash(batch.RelativePath)),
		},
	}}

	// Phase 1: another process holds the repository.
	h.engine.checkErr = domain.NewError(domain.ErrClassUnavailable, "restic_repository_locked",
		"the repository check could not proceed because the repository is locked by another operation")
	unmountsBefore := h.media.unmounts

	runID, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	refused := h.run(runID)
	if refused.Status.Successful() {
		t.Fatal("recovery succeeded against a repository held by another process")
	}
	if refused.ErrorCode != "restic_repository_locked" {
		t.Fatalf("refused recovery error code = %q, want restic_repository_locked", refused.ErrorCode)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryRecoveryRequired {
		t.Fatalf("repository health = %s, want RECOVERY_REQUIRED", health)
	}
	if still := h.run(interrupted); still.Status.Successful() {
		t.Fatal("a locked repository still adopted the interrupted backup")
	}
	if backup := h.backupRun(interrupted); backup.VerifiedSnapshotID != "" ||
		backup.Durability == domain.DurabilityAdopted {
		t.Fatalf("a locked repository granted durability: %#v", backup)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a locked repository granted revision coverage")
	}
	// Refusing is not an excuse to leave the media mounted.
	if h.media.unmounts <= unmountsBefore {
		t.Fatal("the disk was not synced and unmounted after the refusal")
	}

	// Phase 2: the owning process exits and its lock goes with it. Nothing about
	// the repository changed, so the same recovery must now go through.
	h.engine.checkErr = nil

	secondID, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("start second recovery: %v", err)
	}
	if run := h.run(secondID); !run.Status.Successful() {
		t.Fatalf("recovery did not resume once the lock was released: %s / %s", run.Status, run.SafeError)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
		t.Fatalf("repository health after the lock cleared = %s, want HEALTHY", health)
	}
	adopted := h.run(interrupted)
	if !adopted.Status.Successful() {
		t.Fatalf("the interrupted run was not adopted after the lock cleared: %s", adopted.Status)
	}
	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 || coverage[0].BackupRunID != interrupted ||
		coverage[0].SnapshotID != testSnapshotID {
		t.Fatalf("coverage after the lock cleared = %#v", coverage)
	}
}

// Recovery is explicit. It never re-runs the backup that was interrupted.
func TestRecoveryNeverRerunsABackup(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	source := h.seedSource("documents", 1)
	ctx := context.Background()

	h.startStuckBackup(source, domain.StageResticRunning, "")
	if _, err := h.service.Recover(ctx); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	if _, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID); err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if h.engine.called("backup") {
		t.Fatal("recovery re-ran a backup")
	}
}

// A repository left mid-establishment by a crash is suspect on its own account.
func TestStartupRecoverySuspectsAnUnfinishedInitialization(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	ctx := context.Background()
	if err := h.store.SetRepositoryHealth(ctx, h.repository.ID, domain.RepositoryInitializing, ""); err != nil {
		t.Fatalf("set health: %v", err)
	}

	report, err := h.service.Recover(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.RepositoriesSuspected != 1 {
		t.Fatalf("repositories suspected = %d, want 1", report.RepositoriesSuspected)
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health = %s, want SUSPECT", health)
	}
}

// ----------------------------------------------------------------- read model

func TestEligibleSourcesExplainsIneligibility(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.establishRepository()
	stable := h.seedSource("documents", 1)
	broken := h.seedSource("photos", 1)
	if err := h.store.SetSourceStagingStatus(context.Background(), broken.ID,
		domain.StagingRecoveryRequired); err != nil {
		t.Fatalf("set staging status: %v", err)
	}

	sources, err := h.service.EligibleSources(context.Background())
	if err != nil {
		t.Fatalf("eligible sources: %v", err)
	}
	byID := make(map[string]EligibleSource, len(sources))
	for _, source := range sources {
		byID[source.Source.ID] = source
	}
	if !byID[stable.ID].Eligible {
		t.Fatal("a stable source was reported as ineligible")
	}
	if byID[broken.ID].Eligible {
		t.Fatal("a source needing recovery was reported as eligible")
	}
	if byID[broken.ID].Reason == "" {
		t.Fatal("an ineligible source was listed with no explanation")
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
