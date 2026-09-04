package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
	"github.com/bejusxd/rotup/internal/system"
)

// setupFixture builds a handler over fakes so onboarding state can be driven
// from persisted state alone.
type setupFixture struct {
	backend *fakeBackend
	media   *fakeMedia
	backups *fakeBackups
	handler http.Handler
	report  system.Report
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	fixture := &setupFixture{
		backend: newFakeBackend(),
		media:   &fakeMedia{available: true, snapshot: removable.Snapshot{PlatformAvailable: true}},
		backups: &fakeBackups{},
	}
	handler, err := NewHandler(Deps{
		Version: "dev",
		Status:  fixture.backend,
		Repo:    fixture.backend,
		Media:   fixture.media,
		Backups: fixture.backups,
		SystemInspect: func(context.Context) system.Report {
			return fixture.report
		},
		Engine:       func() EngineStatus { return EngineStatus{Name: "rclone", Available: true} },
		BackupEngine: func() EngineStatus { return EngineStatus{Name: "restic", Available: true} },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	fixture.handler = handler
	return fixture
}

func (f *setupFixture) state(t *testing.T) setupDTO {
	t.Helper()
	w := apiRequest(t, f.handler, "GET", "/api/v1/setup", "", false)
	if w.Code != 200 {
		t.Fatalf("GET /api/v1/setup = %d: %s", w.Code, w.Body.String())
	}
	var state setupDTO
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return state
}

func step(t *testing.T, state setupDTO, id setupStepID) setupStepDTO {
	t.Helper()
	for _, s := range state.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %q in %v", id, state.Steps)
	return setupStepDTO{}
}

func TestSetupStateOnAFreshInstallPointsAtTheFirstAction(t *testing.T) {
	fixture := newSetupFixture(t)

	state := fixture.state(t)

	if state.Complete {
		t.Fatal("a fresh install must not report onboarding as complete")
	}
	if state.NextStep == nil || state.NextStep.ID != stepSource {
		t.Fatalf("next step = %v, want the source step", state.NextStep)
	}
	if state.NextStep.Href != "/sources/new" {
		t.Errorf("next step href = %q, want the page that performs it", state.NextStep.Href)
	}
	for _, s := range state.Steps {
		if s.Done {
			t.Errorf("step %s reported done on an empty installation", s.ID)
		}
		if s.Evidence == "" {
			t.Errorf("step %s gives no evidence for its state", s.ID)
		}
	}
}

// Each of these is a distinction a frontend deriving its own state gets wrong.
// The backend owns them, so they are asserted here.
func TestSetupStateNeverInfersACompletedStepFromAWeakerFact(t *testing.T) {
	t.Run("a configured source is not a reachable one", func(t *testing.T) {
		fixture := newSetupFixture(t)
		fixture.backend.sources = []domain.Source{{ID: "source-1", Name: "Photos", Enabled: true}}

		state := fixture.state(t)

		if !step(t, state, stepSource).Done {
			t.Error("the source step should be done")
		}
		if step(t, state, stepConnection).Done {
			t.Error("a source that exists must not count as a tested connection")
		}
		if !strings.Contains(step(t, state, stepConnection).Evidence, "not proof one never happened") {
			t.Errorf("bounded-history evidence must say what it does not prove: %q",
				step(t, state, stepConnection).Evidence)
		}
	})

	t.Run("a collection run is not a published local copy", func(t *testing.T) {
		fixture := newSetupFixture(t)
		fixture.backend.sources = []domain.Source{{ID: "source-1", Name: "Photos", Enabled: true}}
		// A collection run that finished but never published leaves the source
		// with no published state, and that is what decides the step.
		fixture.backend.runs = []domain.Run{{
			ID: "run-1", Kind: domain.RunKindCollection, SourceID: "source-1",
			Status: domain.RunStatusSucceeded,
		}}

		if step(t, fixture.state(t), stepCollection).Done {
			t.Error("a successful collection run without a published copy must not complete the step")
		}
	})

	t.Run("a repository row is not established storage", func(t *testing.T) {
		fixture := newSetupFixture(t)
		// Established() is false without an establishment timestamp: setup
		// started and did not finish.
		fixture.backups.repositories = []backup.RepositoryView{{
			Repository: domain.Repository{ID: "repo-1", DiskID: "disk-1"},
		}}

		state := fixture.state(t)
		storage := step(t, state, stepStorage)
		if storage.Done {
			t.Error("an unestablished repository must not count as prepared storage")
		}
		if !strings.Contains(storage.Evidence, "not established") {
			t.Errorf("evidence must say the identity is not established: %q", storage.Evidence)
		}
	})

	t.Run("a registered disk is not a connected one", func(t *testing.T) {
		fixture := newSetupFixture(t)
		established := time.Now().UTC()
		fixture.backups.repositories = []backup.RepositoryView{{
			Repository: domain.Repository{ID: "repo-1", DiskID: "disk-1",
				ResticRepositoryID: strings.Repeat("a", 64), EstablishedAt: &established},
			Disk:      &domain.Disk{ID: "disk-1", Name: "Backup A"},
			Connected: false,
		}}

		state := fixture.state(t)
		if !step(t, state, stepStorage).Done {
			t.Error("established storage should complete the storage step")
		}
		connected := step(t, state, stepDiskConnected)
		if connected.Done {
			t.Error("a registered disk that is not present must not count as connected")
		}
		if connected.Href != "/disks/disk-1" {
			t.Errorf("the connect step must point at that disk, got %q", connected.Href)
		}
	})

	t.Run("a candidate snapshot is not a completed backup", func(t *testing.T) {
		fixture := newSetupFixture(t)
		established := time.Now().UTC()
		fixture.backups.repositories = []backup.RepositoryView{{
			// A last snapshot id with no durable success timestamp is exactly
			// the state a crashed backup leaves behind.
			Repository: domain.Repository{ID: "repo-1", DiskID: "disk-1",
				ResticRepositoryID: strings.Repeat("a", 64),
				EstablishedAt:      &established, LastSnapshotID: "candidate-only"},
			Disk:      &domain.Disk{ID: "disk-1", Name: "Backup A"},
			Connected: true,
		}}

		state := fixture.state(t)
		backupStep := step(t, state, stepBackup)
		if backupStep.Done {
			t.Error("a snapshot identifier must not complete the backup step")
		}
		if !strings.Contains(backupStep.Evidence, "is not a backup") {
			t.Errorf("evidence must distinguish a local copy from a backup: %q", backupStep.Evidence)
		}
		if state.Complete {
			t.Error("onboarding is not complete without a durable backup")
		}
	})
}

func TestSetupStateReportsCompletionOnlyFromDurableSuccess(t *testing.T) {
	fixture := newSetupFixture(t)
	published := time.Now().UTC()
	fixture.backend.sources = []domain.Source{{
		ID: "source-1", Name: "Photos", Enabled: true,
		CredentialRefID: "ref-1", LastPublishedAt: &published,
	}}
	fixture.backend.runs = []domain.Run{{
		ID: "run-1", Kind: domain.RunKindConnectionTest, SourceID: "source-1",
		Status: domain.RunStatusSucceeded, FinishedAt: published,
	}}
	fixture.media.snapshot = removable.Snapshot{PlatformAvailable: true, Views: []removable.MediaView{{
		Disk: testDisk(), Status: domain.DiskAvailableUnmounted, Supported: true,
	}}}
	fixture.backups.repositories = []backup.RepositoryView{{
		Repository: domain.Repository{ID: "repo-1", DiskID: testDiskID,
			ResticRepositoryID:     strings.Repeat("a", 64),
			EstablishedAt:          &published,
			LastSuccessfulBackupAt: &published},
		Disk:      testDisk(),
		Connected: true,
	}}

	state := fixture.state(t)

	for _, s := range state.Steps {
		if !s.Done {
			t.Errorf("step %s not done: %s", s.ID, s.Evidence)
		}
	}
	if !state.Complete || state.NextStep != nil {
		t.Fatalf("complete=%v nextStep=%v, want a finished onboarding", state.Complete, state.NextStep)
	}
}

func TestSetupStateSeparatesSystemProvisioningFromApplicationOnboarding(t *testing.T) {
	fixture := newSetupFixture(t)
	fixture.report.Fail(system.GroupHelper, "helper.installed", "Mount helper",
		"/usr/libexec/rotup-mounthelper is not installed", "Run `sudo rotupd setup`.")
	fixture.report.Warn(system.GroupTools, "tools.restic", "restic", "not found", "Install restic.")

	state := fixture.state(t)

	if state.System.Ready {
		t.Fatal("a host with a missing helper must not report system readiness")
	}
	if !state.System.Checked {
		t.Error("the daemon inspected the host, so Checked must be true")
	}
	if state.System.Command != "sudo rotupd setup" {
		t.Errorf("the UI must point at the CLI, got %q", state.System.Command)
	}
	if len(state.System.Issues) != 2 {
		t.Fatalf("issues = %d, want the error and the warning", len(state.System.Issues))
	}
	// Most severe first, and the error marked as the one that blocks.
	if !state.System.Issues[0].Critical || state.System.Issues[0].ID != "helper.installed" {
		t.Errorf("issues must be ranked with the blocking one first: %+v", state.System.Issues)
	}
	if state.System.Issues[1].Critical {
		t.Error("a warning must not be reported as critical")
	}
	// System problems never mark an application step done or not done; they are
	// a separate axis with a separate remedy.
	if step(t, state, stepSource).Done {
		t.Error("system state must not affect application steps")
	}
}

func TestSetupStateSaysWhenItCouldNotCheckTheHost(t *testing.T) {
	handler, err := NewHandler(Deps{
		Version: "dev",
		Status:  newFakeBackend(),
		Repo:    newFakeBackend(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(t, handler, "GET", "/api/v1/setup", "", false)
	var state setupDTO
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	// With no inspector wired in, the honest answer is "not checked", not a
	// confident claim that the host is fine.
	if state.System.Checked {
		t.Error("a daemon with no inspector must report that it did not check")
	}
	if state.System.MediaAvailable {
		t.Error("a daemon with no media seam must not report media as available")
	}
}

func TestSetupStateExposesNoSecretMaterial(t *testing.T) {
	fixture := newSetupFixture(t)
	fixture.backend.sources = []domain.Source{{
		ID: "source-1", Name: "Photos", Enabled: true,
		CredentialRefID: "SECRET-REFERENCE-CANARY",
	}}
	fixture.report.Fail(system.GroupData, "data.secrets", "Secret store",
		"/var/lib/rotup/secrets is mode 0755", "Run chmod 0700.")

	w := apiRequest(t, fixture.handler, "GET", "/api/v1/setup", "", false)

	// "password" is legitimate operator prose here ("Share password saved"), so
	// the assertion is on the material itself and on the fields that would
	// carry it, never on the English word.
	for _, forbidden := range []string{"SECRET-REFERENCE-CANARY", "credentialRef", "\"password\"", "passwordValue"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("setup state leaked %q:\n%s", forbidden, w.Body.String())
		}
	}
	// The permission finding is still reported; only the material is private.
	if !strings.Contains(w.Body.String(), "Secret store") {
		t.Error("a secret-store permission problem must still be surfaced")
	}
}

func TestSetupStateIsReadOnlyAndRefusesMutation(t *testing.T) {
	fixture := newSetupFixture(t)

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		w := apiRequest(t, fixture.handler, method, "/api/v1/setup", "{}", true)
		if w.Code < 400 {
			t.Errorf("%s /api/v1/setup = %d; the setup endpoint must be read-only", method, w.Code)
		}
	}
	// Nothing may have been started by reading it.
	fixture.state(t)
	if len(fixture.backend.startedRuns) != 0 || len(fixture.backend.startedTests) != 0 {
		t.Error("reading setup state started an operation")
	}
	if len(fixture.backups.setups) != 0 || len(fixture.backups.backups) != 0 {
		t.Error("reading setup state started a repository operation")
	}
	if len(fixture.media.registered) != 0 || len(fixture.media.mounted) != 0 {
		t.Error("reading setup state touched media")
	}
}
