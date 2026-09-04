package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

const (
	testRepositoryID     = "33333333-3333-4333-8333-333333333333"
	testResticRepository = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSnapshot         = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	uiRepositoryPassword = "an-adequately-long-password"
)

// fakeBackups records what the repository pages asked for. It performs no
// repository work, so the handlers can be exercised on any platform without
// restic installed.
type fakeBackups struct {
	mu sync.Mutex

	repositories []backup.RepositoryView
	sources      []backup.EligibleSource
	history      []backup.BackupHistoryEntry

	setupErr    error
	backupErr   error
	recoveryErr error

	setups    []backup.SetupInput
	backups   []startedBackup
	recovered []string
	// contexts records the context each operation ran under, so the test can
	// prove a repository operation is not tied to the HTTP request.
	contexts []context.Context
}

type startedBackup struct {
	RepositoryID string
	SourceIDs    []string
}

func (f *fakeBackups) ListRepositories(context.Context) ([]backup.RepositoryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repositories, nil
}

func (f *fakeBackups) RepositoryDetail(_ context.Context, id string) (backup.RepositoryView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.repositories {
		if item.Repository.ID == id {
			return item, nil
		}
	}
	return backup.RepositoryView{}, domain.NewError(domain.ErrClassConfiguration,
		"repository_missing", "that repository does not exist")
}

func (f *fakeBackups) BackupHistory(context.Context, string, int) ([]backup.BackupHistoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.history, nil
}

func (f *fakeBackups) EligibleSources(context.Context) ([]backup.EligibleSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sources, nil
}

func (f *fakeBackups) StartRepositorySetup(ctx context.Context, input backup.SetupInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	if f.setupErr != nil {
		return "", f.setupErr
	}
	f.setups = append(f.setups, input)
	return "run-setup", nil
}

func (f *fakeBackups) StartBackup(ctx context.Context, repositoryID string, sourceIDs []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	if f.backupErr != nil {
		return "", f.backupErr
	}
	f.backups = append(f.backups, startedBackup{RepositoryID: repositoryID, SourceIDs: sourceIDs})
	return "run-backup", nil
}

func (f *fakeBackups) StartRepositoryRecovery(ctx context.Context, repositoryID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	if f.recoveryErr != nil {
		return "", f.recoveryErr
	}
	f.recovered = append(f.recovered, repositoryID)
	return "run-recover", nil
}

func testRepositoryView(health domain.RepositoryHealth) backup.RepositoryView {
	established := time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC)
	return backup.RepositoryView{
		Repository: domain.Repository{
			ID:                 testRepositoryID,
			DiskID:             testDiskID,
			FilesystemUUID:     "11111111-2222-3333-4444-555555555555",
			RelativePath:       domain.RepositoryRelativePath,
			ResticRepositoryID: testResticRepository,
			Origin:             domain.RepositoryInitialized,
			Health:             health,
			EstablishedAt:      &established,
			LastSnapshotID:     testSnapshot,
		},
		Disk:        testDisk(),
		Connected:   true,
		HasPassword: true,
	}
}

func testEligibleSource(eligible bool) backup.EligibleSource {
	source := domain.Source{
		ID: testSourceID, Name: "documents", Enabled: true,
		StagingStatus: domain.StagingStable, StagingStateVersion: 5,
	}
	entry := backup.EligibleSource{Source: source, Eligible: eligible}
	if !eligible {
		entry.Source.StagingStatus = domain.StagingRecoveryRequired
		entry.Reason = "source \"documents\" needs recovery"
	}
	return entry
}

// newBackupServer builds a server whose repository seam is the fake.
func newBackupServer(t *testing.T, backups *fakeBackups, media *fakeMedia) *testServer {
	t.Helper()
	server := newTestServer(t)
	if media == nil {
		media = &fakeMedia{available: true}
	}

	handler, err := NewHandler(Deps{
		Version: "dev",
		Status:  server.backend,
		Repo:    server.backend,
		Admin:   server.backend,
		Runner:  server.backend,
		Staging: server.layout,
		Media:   media,
		Backups: backups,
		Engine:  func() EngineStatus { return EngineStatus{Name: "rclone", Available: true} },
		BackupEngine: func() EngineStatus {
			return EngineStatus{Name: "restic", Version: "0.17.3", Available: true}
		},
		Background: func() context.Context {
			return context.WithValue(context.Background(), backgroundMarker{}, true)
		},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	server.handler = handler
	server.token = server.fetchToken(t, "/sources/new")
	return server
}

func TestRepositoriesPageShowsIdentityAndHealth(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{repositories: []backup.RepositoryView{
		testRepositoryView(domain.RepositoryHealthy),
	}}
	server := newBackupServer(t, backups, nil)

	response := server.get(t, "/repositories")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, fragment := range []string{
		"backup A", "11111111-2222-3333-4444-555555555555", "HEALTHY",
		testResticRepository[:8], "restic", "0.17.3",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the repositories page does not contain %q", fragment)
		}
	}
	// M4 has no retention, so it must offer nothing that mutates history.
	for _, forbidden := range []string{"Forget", "Prune", "Delete snapshot", "Retention"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the repositories page offers %q, which is not an M4 action", forbidden)
		}
	}
}

// A registered disk with no repository is where establishing one is offered,
// and only there.
func TestRepositoriesPageOffersSetupForUnclaimedDisk(t *testing.T) {
	t.Parallel()

	media := &fakeMedia{available: true, snapshot: removable.Snapshot{
		PlatformAvailable: true,
		Views: []removable.MediaView{
			{Disk: testDisk(), Status: domain.DiskAvailableUnmounted, Supported: true},
			// Unregistered media must never be offered a repository.
			{Observed: testObservation("/dev/sdc1", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ext4"),
				Status: domain.DiskDetected, Supported: true},
		},
	}}
	server := newBackupServer(t, &fakeBackups{}, media)

	body := server.get(t, "/repositories").Body.String()
	if !strings.Contains(body, "/disks/"+testDiskID+"/repository/init") {
		t.Fatal("the page does not offer initializing a repository on the registered disk")
	}
	if !strings.Contains(body, "/disks/"+testDiskID+"/repository/adopt") {
		t.Fatal("the page does not offer adopting an existing repository")
	}
	if strings.Contains(body, "/dev/sdc1") {
		t.Fatal("unregistered media appeared on the repositories page")
	}
	// The escrow requirement has to be stated where the decision is made.
	for _, fragment := range []string{"cannot reset or recover", "outside this host"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the setup form does not state %q", fragment)
		}
	}
}

func TestFailedSetupStillOffersExplicitAdoption(t *testing.T) {
	t.Parallel()
	media := &fakeMedia{available: true, snapshot: removable.Snapshot{
		PlatformAvailable: true,
		Views:             []removable.MediaView{{Disk: testDisk(), Status: domain.DiskAvailableUnmounted, Supported: true}},
	}}
	backups := &fakeBackups{repositories: []backup.RepositoryView{{
		Repository: domain.Repository{ID: testRepositoryID, DiskID: testDiskID, Health: domain.RepositorySuspect},
		Disk:       testDisk(), Connected: true,
	}}}
	server := newBackupServer(t, backups, media)
	if body := server.get(t, "/repositories").Body.String(); !strings.Contains(body, "/disks/"+testDiskID+"/repository/adopt") {
		t.Fatal("an interrupted setup stranded its repository with no explicit adoption action")
	}
}

func TestRepositoryInitializationPostsPasswordAndEscrow(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{}
	server := newBackupServer(t, backups, nil)

	response := server.post(t, "/disks/"+testDiskID+"/repository/init", url.Values{
		"repository_password": {uiRepositoryPassword},
		"escrow_acknowledged": {"1"},
	}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the run page", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/runs/run-setup" {
		t.Fatalf("redirect = %q", location)
	}
	if len(backups.setups) != 1 {
		t.Fatalf("setups = %v", backups.setups)
	}
	setup := backups.setups[0]
	if setup.DiskID != testDiskID || setup.Mode != backup.SetupInitialize {
		t.Fatalf("setup = %#v", setup)
	}
	if setup.Password != uiRepositoryPassword || !setup.EscrowAcknowledged {
		t.Fatalf("the form did not reach the use case intact: %#v", setup)
	}
	// The operation must outlive the request that started it.
	if backups.contexts[0].Value(backgroundMarker{}) != true {
		t.Fatal("repository setup ran on the request context")
	}
}

func TestRepositoryAdoptionUsesAdoptMode(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{}
	server := newBackupServer(t, backups, nil)

	server.post(t, "/disks/"+testDiskID+"/repository/adopt", url.Values{
		"repository_password": {uiRepositoryPassword},
		"escrow_acknowledged": {"1"},
	}, true)
	if len(backups.setups) != 1 || backups.setups[0].Mode != backup.SetupAdopt {
		t.Fatalf("setups = %v", backups.setups)
	}
}

func TestRepositorySetupRequiresCSRFToken(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{}
	server := newBackupServer(t, backups, nil)

	response := server.post(t, "/disks/"+testDiskID+"/repository/init", url.Values{
		"repository_password": {uiRepositoryPassword},
		"escrow_acknowledged": {"1"},
	}, false)
	if response.Code == http.StatusSeeOther {
		t.Fatal("a repository was established without a form token")
	}
	if len(backups.setups) != 0 {
		t.Fatal("the use case ran despite a missing form token")
	}
}

func TestManualBackupPostsSelectedSources(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryHealthy)},
		sources:      []backup.EligibleSource{testEligibleSource(true)},
	}
	server := newBackupServer(t, backups, nil)

	response := server.post(t, "/repositories/"+testRepositoryID+"/backup", url.Values{
		"source_id": {testSourceID},
	}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/runs/run-backup" {
		t.Fatalf("redirect = %q", location)
	}
	if len(backups.backups) != 1 {
		t.Fatalf("backups = %v", backups.backups)
	}
	if backups.backups[0].RepositoryID != testRepositoryID ||
		len(backups.backups[0].SourceIDs) != 1 ||
		backups.backups[0].SourceIDs[0] != testSourceID {
		t.Fatalf("backup request = %#v", backups.backups[0])
	}
	if backups.contexts[0].Value(backgroundMarker{}) != true {
		t.Fatal("the backup ran on the request context")
	}
}

func TestManualBackupRequiresCSRFToken(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryHealthy)},
	}
	server := newBackupServer(t, backups, nil)

	response := server.post(t, "/repositories/"+testRepositoryID+"/backup",
		url.Values{"source_id": {testSourceID}}, false)
	if response.Code == http.StatusSeeOther {
		t.Fatal("a backup started without a form token")
	}
	if len(backups.backups) != 0 {
		t.Fatal("the use case ran despite a missing form token")
	}
}

// A suspect repository must not offer a backup, and must say why.
func TestSuspectRepositoryPageRefusesBackupAndOffersRecovery(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositorySuspect)},
		sources:      []backup.EligibleSource{testEligibleSource(true)},
	}
	server := newBackupServer(t, backups, nil)

	body := server.get(t, "/repositories/"+testRepositoryID).Body.String()
	if strings.Contains(body, "Start backup") {
		t.Fatal("a suspect repository offered a backup")
	}
	if !strings.Contains(body, "SUSPECT") {
		t.Fatal("the page does not state that the repository is suspect")
	}
	if !strings.Contains(body, "/repositories/"+testRepositoryID+"/recover") {
		t.Fatal("the page does not offer a recovery")
	}
}

func TestRepositoryPageExplainsIneligibleSource(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryHealthy)},
		sources:      []backup.EligibleSource{testEligibleSource(false)},
	}
	server := newBackupServer(t, backups, nil)

	body := server.get(t, "/repositories/"+testRepositoryID).Body.String()
	if !strings.Contains(body, "needs recovery") {
		t.Fatal("the page does not explain why a source cannot be backed up")
	}
	if strings.Contains(body, `name="source_id" value="`+testSourceID+`"`) {
		t.Fatal("an ineligible source was offered as a backup selection")
	}
}

func TestRepositoryRecoveryPosts(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositorySuspect)},
	}
	server := newBackupServer(t, backups, nil)

	response := server.post(t, "/repositories/"+testRepositoryID+"/recover", url.Values{}, true)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	if len(backups.recovered) != 1 || backups.recovered[0] != testRepositoryID {
		t.Fatalf("recovered = %v", backups.recovered)
	}
}

// A repository password is stored once and never shown again. The pages must
// carry no trace of one.
func TestRepositoryPagesNeverRenderAPassword(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryHealthy)},
		sources:      []backup.EligibleSource{testEligibleSource(true)},
	}
	server := newBackupServer(t, backups, nil)

	for _, path := range []string{"/repositories", "/repositories/" + testRepositoryID} {
		body := server.get(t, path).Body.String()
		if strings.Contains(body, uiRepositoryPassword) {
			t.Fatalf("%s rendered a repository password", path)
		}
		// A password input must never be pre-filled from stored state.
		if strings.Contains(body, `type="password"`) && strings.Contains(body, `value="`+uiRepositoryPassword) {
			t.Fatalf("%s pre-filled a password field", path)
		}
	}
}

func TestBackupHistoryShowsDurabilityAndSnapshot(t *testing.T) {
	t.Parallel()

	finished := time.Date(2026, time.September, 4, 11, 0, 0, 0, time.UTC)
	backups := &fakeBackups{
		repositories: []backup.RepositoryView{testRepositoryView(domain.RepositoryHealthy)},
		history: []backup.BackupHistoryEntry{
			{
				Run: domain.Run{
					ID: "run-1", Kind: domain.RunKindBackupLatest,
					Status: domain.RunStatusSucceeded, RequestedAt: finished, FinishedAt: finished,
				},
				Backup: domain.BackupRun{
					RunID: "run-1", Stage: domain.StageDurableSuccess,
					Durability: domain.DurabilityNormal, VerifiedSnapshotID: testSnapshot,
					FilesProcessed: 11, BytesProcessed: 4096,
				},
			},
			{
				Run: domain.Run{
					ID: "run-2", Kind: domain.RunKindBackupLatest,
					Status: domain.RunStatusInterrupted, RequestedAt: finished, FinishedAt: finished,
				},
				Backup: domain.BackupRun{
					RunID: "run-2", Stage: domain.StageSnapshotCreated,
					Durability: domain.DurabilityUnproven, CandidateSnapshotID: testSnapshot,
				},
			},
		},
	}
	server := newBackupServer(t, backups, nil)

	body := server.get(t, "/repositories/"+testRepositoryID).Body.String()
	for _, fragment := range []string{
		"DURABLE_SUCCESS", "SNAPSHOT_CREATED", testSnapshot[:8], "unverified",
		"/runs/run-1", "/runs/run-2",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the backup history does not contain %q", fragment)
		}
	}
}

// A repository that does not exist must not render a page that implies it does.
func TestUnknownRepositoryIsRefused(t *testing.T) {
	t.Parallel()

	server := newBackupServer(t, &fakeBackups{}, nil)
	response := server.get(t, "/repositories/"+testRepositoryID)
	if response.Code == http.StatusOK {
		t.Fatalf("an unknown repository rendered with status %d", response.Code)
	}
}

// The M1-M3 routes must keep working with the backup seam wired in.
func TestExistingRoutesStillRespondWithBackupsWired(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{repositories: []backup.RepositoryView{
		testRepositoryView(domain.RepositoryHealthy),
	}}
	media := &fakeMedia{available: true, snapshot: removable.Snapshot{PlatformAvailable: true}}
	server := newBackupServer(t, backups, media)

	for _, path := range []string{"/", "/healthz", "/sources", "/runs", "/disks", "/repositories"} {
		response := server.get(t, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.Code)
		}
	}
}

// The dashboard reports both engines, because a daemon can have one and not
// the other and the operator has to be able to tell which.
func TestDashboardReportsBothEngines(t *testing.T) {
	t.Parallel()

	backups := &fakeBackups{repositories: []backup.RepositoryView{
		testRepositoryView(domain.RepositoryHealthy),
	}}
	server := newBackupServer(t, backups, nil)

	body := server.get(t, "/").Body.String()
	for _, fragment := range []string{"rclone", "restic", "0.17.3"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("the dashboard does not report %q", fragment)
		}
	}
}
