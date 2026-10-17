package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/removable"
)

var _ TargetStore = (*sqlite.Store)(nil)
var _ TargetMedia = (*removable.Service)(nil)

type targetStore struct {
	repositories []domain.Repository
	unfinished   map[string][]domain.BackupRun
	err          error
	pendingErr   error
}

func (s targetStore) ListRepositories(context.Context) ([]domain.Repository, error) {
	return s.repositories, s.err
}
func (s targetStore) BackupRunsAwaitingRecovery(_ context.Context, id string) ([]domain.BackupRun, error) {
	return s.unfinished[id], s.pendingErr
}

type targetMedia struct {
	snapshot     removable.Snapshot
	reconcileErr error
	snapshotErr  error
	reconciled   bool
}

func (m *targetMedia) Reconcile(context.Context) error { m.reconciled = true; return m.reconcileErr }
func (m *targetMedia) Snapshot(context.Context) (removable.Snapshot, error) {
	if !m.reconciled {
		return removable.Snapshot{}, errors.New("snapshot used without reconciliation")
	}
	return m.snapshot, m.snapshotErr
}

func targetFixture() (targetStore, *targetMedia) {
	var repos []domain.Repository
	var views []removable.MediaView
	for i, id := range []string{"a", "b"} {
		connected := time.Date(2026, 10, 18, 10+i, 0, 0, 0, time.UTC)
		uuid := "12345678-1234-1234-1234-123456789ab" + id
		repos = append(repos, domain.Repository{ID: "repo-" + id, DiskID: id, FilesystemUUID: uuid,
			ResticRepositoryID: "established", Health: domain.RepositoryHealthy})
		views = append(views, removable.MediaView{
			Disk:     &domain.Disk{ID: id, FilesystemUUID: uuid, Enabled: true, LastConnectedAt: &connected},
			Observed: &domain.ObservedMedia{FilesystemUUID: uuid, FilesystemType: "ext4"},
			Status:   domain.DiskAvailableUnmounted, Supported: true,
		})
	}
	return targetStore{repositories: repos, unfinished: make(map[string][]domain.BackupRun)},
		&targetMedia{snapshot: removable.Snapshot{PlatformAvailable: true, Views: views}}
}

func TestTargetPrefersLatestConnectionNotEnumerationOrBackupOrder(t *testing.T) {
	store, media := targetFixture()
	newestBackup := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	store.repositories[0].LastSuccessfulBackupAt = &newestBackup
	media.snapshot.Views[0].Disk.FirstRegisteredAt = newestBackup
	media.snapshot.Views[0].Observed.DevicePath = "/dev/sdz1"
	media.snapshot.Views[1].Observed.DevicePath = "/dev/loop1"
	for i := 0; i < 2; i++ {
		target, err := SelectBackupTarget(context.Background(), store, media)
		if err != nil || target == nil || target.RepositoryID != "repo-b" {
			t.Fatalf("target=%+v err=%v", target, err)
		}
		store.repositories[0], store.repositories[1] = store.repositories[1], store.repositories[0]
		media.snapshot.Views[0], media.snapshot.Views[1] = media.snapshot.Views[1], media.snapshot.Views[0]
	}
}

func TestUnsafeOrUnavailableNewerTargetCannotWin(t *testing.T) {
	for name, mutate := range map[string]func(*targetStore, *removable.MediaView){
		"absent":                 func(_ *targetStore, v *removable.MediaView) { v.Observed = nil },
		"unregistered":           func(_ *targetStore, v *removable.MediaView) { v.Disk = nil },
		"disabled":               func(_ *targetStore, v *removable.MediaView) { v.Disk.Enabled = false },
		"busy":                   func(_ *targetStore, v *removable.MediaView) { v.Busy = true },
		"ambiguous":              func(_ *targetStore, v *removable.MediaView) { v.Ambiguous = true },
		"unsupported":            func(_ *targetStore, v *removable.MediaView) { v.Supported = false },
		"readonly":               func(_ *targetStore, v *removable.MediaView) { v.Observed.ReadOnly = true },
		"mounted":                func(_ *targetStore, v *removable.MediaView) { v.Status = domain.DiskReady },
		"external mount":         func(_ *targetStore, v *removable.MediaView) { v.ExternalMounts = []string{"/mnt/external"} },
		"error":                  func(_ *targetStore, v *removable.MediaView) { v.ErrorCode = domain.DiskErrorMountFailed },
		"wrong observation UUID": func(_ *targetStore, v *removable.MediaView) { v.Observed.FilesystemUUID = "different" },
		"wrong repository UUID":  func(s *targetStore, _ *removable.MediaView) { s.repositories[1].FilesystemUUID = "different" },
		"suspect":                func(s *targetStore, _ *removable.MediaView) { s.repositories[1].Health = domain.RepositorySuspect },
		"not established":        func(s *targetStore, _ *removable.MediaView) { s.repositories[1].ResticRepositoryID = "" },
		"unfinished write": func(s *targetStore, _ *removable.MediaView) {
			s.unfinished["repo-b"] = []domain.BackupRun{{RunID: "unconfirmed"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, media := targetFixture()
			mutate(&store, &media.snapshot.Views[1])
			target, err := SelectBackupTarget(context.Background(), store, media)
			if err != nil || target == nil || target.RepositoryID != "repo-a" {
				t.Fatalf("target=%+v err=%v", target, err)
			}
		})
	}
}

func TestTargetTiesAreDeterministicAndNoTargetDoesNotStartAnAttempt(t *testing.T) {
	store, media := targetFixture()
	media.snapshot.Views[1].Disk.LastConnectedAt = media.snapshot.Views[0].Disk.LastConnectedAt
	target, err := SelectBackupTarget(context.Background(), store, media)
	if err != nil || target == nil || target.RepositoryID != "repo-a" {
		t.Fatalf("tie: %+v %v", target, err)
	}
	media.snapshot.Views[0].Disk.LastConnectedAt = nil
	target, err = SelectBackupTarget(context.Background(), store, media)
	if err != nil || target == nil || target.RepositoryID != "repo-b" {
		t.Fatalf("unknown: %+v %v", target, err)
	}
	media.snapshot.Views = nil
	target, err = SelectBackupTarget(context.Background(), store, media)
	if err != nil || target != nil {
		t.Fatalf("no connected disks: %+v %v", target, err)
	}
}

func TestTargetObservationAndPersistenceErrorsFailClosed(t *testing.T) {
	for _, failure := range []string{"reconcile", "snapshot", "enumeration", "platform", "store", "pending writes"} {
		t.Run(failure, func(t *testing.T) {
			store, media := targetFixture()
			injected := errors.New("injected")
			switch failure {
			case "reconcile":
				media.reconcileErr = injected
			case "snapshot":
				media.snapshotErr = injected
			case "enumeration":
				media.snapshot.EnumerationError = "unavailable"
			case "platform":
				media.snapshot.PlatformAvailable = false
			case "store":
				store.err = injected
			case "pending writes":
				store.pendingErr = injected
			}
			if target, err := SelectBackupTarget(context.Background(), store, media); err == nil || target != nil {
				t.Fatalf("unproven target admitted: %+v, %v", target, err)
			}
		})
	}
}

// Use the real media service and SQLite connection history here. Privileged
// operations are deliberately unimplemented: selecting a target never mounts.
type selectionSystem struct {
	ports.MountManager
	devices []domain.ObservedMedia
}

func (s *selectionSystem) Enumerate(context.Context) ([]domain.ObservedMedia, error) {
	return s.devices, nil
}
func (s *selectionSystem) Observe(context.Context) ([]domain.MountObservation, error) {
	return nil, nil
}
func (s *selectionSystem) Available() bool                         { return true }
func (s *selectionSystem) MountPointFor(id string) (string, error) { return "/mnt/rotup/" + id, nil }

func TestSelectionFollowsObservedReconnectWithRealMediaHistory(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "history.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store, observations := targetFixture()
	a, b := *observations.snapshot.Views[0].Observed, *observations.snapshot.Views[1].Observed
	a.DevicePath, b.DevicePath = "/dev/sdb1", "/dev/sdc1"
	system := &selectionSystem{devices: []domain.ObservedMedia{a}}
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	media := removable.New(removable.Options{Store: db, Enumerator: system, Mounts: system, Logger: logger,
		Now: func() time.Time { return now }})
	idA, err := media.Register(ctx, a.FilesystemUUID, "A")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	system.devices = append(system.devices, b)
	idB, err := media.Register(ctx, b.FilesystemUUID, "B")
	if err != nil {
		t.Fatal(err)
	}
	store.repositories[0].DiskID, store.repositories[1].DiskID = idA, idB
	assertTarget := func(want string) {
		t.Helper()
		target, err := SelectBackupTarget(ctx, store, media)
		if err != nil || target == nil || target.DiskID != want {
			t.Fatalf("target = %+v / %v, want %s", target, err, want)
		}
	}
	assertTarget(idB)
	// Ordinary polls and a device rename do not make the older disk newer.
	now = now.Add(time.Hour)
	system.devices[0].DevicePath = "/dev/loop9"
	assertTarget(idB)
	system.devices = []domain.ObservedMedia{b}
	if err := media.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	system.devices = []domain.ObservedMedia{b, a}
	assertTarget(idA)
	lease, err := media.Acquire(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	assertTarget(idB) // Prefer the newest of the AVAILABLE eligible disks.
	lease.Release()
	assertTarget(idA)
}
