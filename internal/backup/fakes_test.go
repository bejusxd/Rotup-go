package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// fakeEngine is a scripted BackupEngine. Each operation can be told to fail, and
// every call is recorded so a test can prove what the service asked for and in
// what order.
type fakeEngine struct {
	mu sync.Mutex

	repositoryID string
	// snapshots is the repository's contents as the engine reports them.
	snapshots []ports.Snapshot

	initErr     error
	inspectErr  error
	backupErr   error
	snapshotErr error
	checkErr    error
	checkResult ports.CheckResult

	// backupResult is returned from Backup when backupErr is nil.
	backupResult ports.BackupResult
	// recordSnapshot controls whether a successful backup actually adds the
	// snapshot to the repository. Setting it false models restic reporting a
	// snapshot that is not really there.
	recordSnapshot bool
	// onBackup runs while the backup is in flight, so a test can simulate the
	// media disappearing partway through.
	onBackup func()

	calls        []string
	backupPaths  []string
	backupTags   []string
	backupRepoID string
}

func newFakeEngine(repositoryID string) *fakeEngine {
	return &fakeEngine{repositoryID: repositoryID, recordSnapshot: true}
}

func (e *fakeEngine) record(name string) {
	e.mu.Lock()
	e.calls = append(e.calls, name)
	e.mu.Unlock()
}

func (e *fakeEngine) called(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, call := range e.calls {
		if call == name {
			return true
		}
	}
	return false
}

func (e *fakeEngine) Probe(context.Context) (ports.EngineInfo, error) {
	e.record("probe")
	return ports.EngineInfo{Name: "restic", Version: "0.17.3"}, nil
}

func (e *fakeEngine) InitializeRepository(_ context.Context, _ ports.RepositorySpec) (ports.RepositoryInfo, error) {
	e.record("init")
	if e.initErr != nil {
		return ports.RepositoryInfo{}, e.initErr
	}
	return ports.RepositoryInfo{ID: e.repositoryID, Version: 2}, nil
}

func (e *fakeEngine) InspectRepository(_ context.Context, _ ports.RepositorySpec) (ports.RepositoryInfo, error) {
	e.record("inspect")
	if e.inspectErr != nil {
		return ports.RepositoryInfo{}, e.inspectErr
	}
	return ports.RepositoryInfo{ID: e.repositoryID, Version: 2}, nil
}

func (e *fakeEngine) Backup(_ context.Context, request ports.BackupRequest) (ports.BackupResult, error) {
	e.record("backup")
	e.mu.Lock()
	e.backupPaths = append([]string{}, request.Paths...)
	e.backupTags = append([]string{}, request.Tags...)
	e.backupRepoID = request.Repository.Directory
	e.mu.Unlock()

	if e.onBackup != nil {
		e.onBackup()
	}
	if e.backupErr != nil {
		return e.backupResult, e.backupErr
	}
	if e.recordSnapshot {
		e.mu.Lock()
		e.snapshots = append(e.snapshots, ports.Snapshot{
			ID:      e.backupResult.Stats.SnapshotID,
			ShortID: shortID(e.backupResult.Stats.SnapshotID),
			Tags:    append([]string{}, request.Tags...),
			Paths:   append([]string{}, request.Paths...),
		})
		e.mu.Unlock()
	}
	return e.backupResult, nil
}

func (e *fakeEngine) Snapshots(context.Context, ports.RepositorySpec) ([]ports.Snapshot, error) {
	e.record("snapshots")
	if e.snapshotErr != nil {
		return nil, e.snapshotErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ports.Snapshot{}, e.snapshots...), nil
}

func (e *fakeEngine) Check(context.Context, ports.RepositorySpec) (ports.CheckResult, error) {
	e.record("check")
	return e.checkResult, e.checkErr
}

// fakeMedia hands out leases over a directory that stands in for a mounted
// disk. Every failure a real disk can produce is expressible here.
type fakeMedia struct {
	mu sync.Mutex

	mountPoint string
	// leases counts how many leases are outstanding, so a test can prove a
	// second operation on the same disk is refused rather than queued.
	held map[string]bool

	mountErr   error
	unmountErr error
	// present models whether the registered filesystem is still connected.
	present bool
	// vanishOnUnmount models the disk being pulled before the flush.
	vanishOnUnmount bool

	statuses  []domain.DiskStatus
	unmounts  int
	onUnmount func(context.Context)
}

func newFakeMedia(t *testing.T) *fakeMedia {
	t.Helper()
	return newFakeMediaAt(t, t.TempDir())
}

// newFakeMediaAt places the stand-in mount point under a caller-chosen root.
// The crash spike needs this: its worker and its parent are separate processes
// and must agree on where the repository lives, which a per-process temporary
// directory cannot give them.
func newFakeMediaAt(t *testing.T, root string) *fakeMedia {
	t.Helper()
	mountPoint := filepath.Join(root, "media")
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		t.Fatalf("create fake mount point: %v", err)
	}
	return &fakeMedia{mountPoint: mountPoint, held: make(map[string]bool), present: true}
}

func (m *fakeMedia) Acquire(_ context.Context, diskID string) (ports.DiskLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[diskID] {
		return nil, domain.NewError(domain.ErrClassUnavailable, "disk_busy",
			"another media operation is already running for this disk")
	}
	m.held[diskID] = true
	return &fakeLease{media: m, diskID: diskID}, nil
}

func (m *fakeMedia) busy(diskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[diskID]
}

type fakeLease struct {
	media   *fakeMedia
	diskID  string
	mounted bool
	done    bool
}

func (l *fakeLease) DiskID() string { return l.diskID }

func (l *fakeLease) MountPoint() (string, error) { return l.media.mountPoint, nil }

func (l *fakeLease) Mounted() bool { return l.mounted }

func (l *fakeLease) Mount(context.Context) error {
	if l.media.mountErr != nil {
		return l.media.mountErr
	}
	l.mounted = true
	return nil
}

func (l *fakeLease) SyncAndUnmount(ctx context.Context) error {
	if l.media.onUnmount != nil {
		l.media.onUnmount(ctx)
	}
	l.media.mu.Lock()
	l.media.unmounts++
	if l.media.vanishOnUnmount {
		l.media.present = false
	}
	l.media.mu.Unlock()
	l.mounted = false
	return l.media.unmountErr
}

func (l *fakeLease) Present(context.Context) (bool, error) {
	l.media.mu.Lock()
	defer l.media.mu.Unlock()
	return l.media.present, nil
}

func (l *fakeLease) SetStatus(_ context.Context, status domain.DiskStatus) error {
	l.media.mu.Lock()
	l.media.statuses = append(l.media.statuses, status)
	l.media.mu.Unlock()
	return nil
}

func (l *fakeLease) Release() {
	if l.done {
		return
	}
	l.done = true
	l.media.mu.Lock()
	delete(l.media.held, l.diskID)
	l.media.mu.Unlock()
}

var errEngineFailed = errors.New("the engine failed")
