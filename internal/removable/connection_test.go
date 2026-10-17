package removable

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestConnectionTimeDoesNotFollowOperationOrStatusChanges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "A")
	connected := now
	h.reconcile()
	now = now.Add(time.Hour)
	if err := h.service.Mount(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertConnection(t, h.disk(id), connected)
	if err := h.service.SyncAndUnmount(ctx, id); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	assertConnection(t, h.disk(id), connected)
	h.service.setStatus(ctx, id, domain.DiskError, domain.DiskErrorMountFailed)
	now = now.Add(time.Hour)
	h.reconcile()
	assertConnection(t, h.disk(id), connected)
	if _, err := h.service.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	assertConnection(t, h.disk(id), connected)
	h.system.renameDevice(uuidA, "/dev/sdz1")
	h.reconcile()
	assertConnection(t, h.disk(id), connected)
}

func TestRegistrationUsesFirstObservationNotRegistrationOrder(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.reconcile()
	first := now
	now = now.Add(time.Hour)
	h.system.attach(mediaItem("/dev/sdc1", uuidB, "ext4"))
	h.reconcile()
	second := now
	now = now.Add(time.Hour)
	b := h.register(uuidB, "B")
	a := h.register(uuidA, "A")
	assertConnection(t, h.disk(a), first)
	assertConnection(t, h.disk(b), second)
}

func TestReconnectWhileBusyIsRememberedUntilLeaseRelease(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "A")
	h.reconcile()
	lease, err := h.service.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	h.system.detachUUID(uuidA)
	h.reconcile()
	now = now.Add(time.Hour)
	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))
	h.reconcile()
	reconnected := now
	now = now.Add(time.Hour)
	lease.Release()
	h.reconcile()
	assertConnection(t, h.disk(id), reconnected)
}

func TestRestartAndEnumerationFailureDoNotInventReconnection(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "A")
	connected := now
	now = now.Add(time.Hour)
	h.service = New(Options{Store: h.store, Enumerator: h.system, Mounts: h.system,
		Logger: h.service.logger, Now: func() time.Time { return now }})
	h.reconcile()
	assertConnection(t, h.disk(id), connected)
	h.system.enumerateErr = errors.New("enumeration temporarily unavailable")
	if err := h.service.Reconcile(context.Background()); err == nil {
		t.Fatal("expected enumeration error")
	}
	h.system.enumerateErr = nil
	now = now.Add(time.Hour)
	h.reconcile()
	assertConnection(t, h.disk(id), connected)
	h.system.detachUUID(uuidA)
	h.reconcile()
	now = now.Add(time.Hour)
	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))
	h.reconcile()
	assertConnection(t, h.disk(id), now)
}

func assertConnection(t *testing.T, disk domain.Disk, want time.Time) {
	t.Helper()
	if disk.LastConnectedAt == nil || !disk.LastConnectedAt.Equal(want) {
		t.Fatalf("connection time = %v, want %v", disk.LastConnectedAt, want)
	}
}

func TestSameObservationTiesAndUUIDCaseDoesNotInventConnection(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.system.attach(mediaItem("/dev/sdc1", uuidB, "ext4"))
	a := h.register(uuidA, "A") // This pass observes both, even before B is registered.
	first := now
	now = now.Add(time.Hour)
	b := h.register(uuidB, "B")
	assertConnection(t, h.disk(a), first)
	assertConnection(t, h.disk(b), first)
	h.system.devices[0].FilesystemUUID = strings.ToUpper(uuidA)
	h.reconcile()
	assertConnection(t, h.disk(a), first)
	// An ambiguous identity remains refused, not treated as a new connection.
	h.system.attach(mediaItem("/dev/sdd1", uuidA, "ext4"))
	h.reconcile()
	if h.disk(a).LastErrorCode != domain.DiskErrorAmbiguousIdentity {
		t.Fatal("duplicate UUID was not refused")
	}
	assertConnection(t, h.disk(a), first)
}

type failingDiskStateStore struct {
	Store
	err error
}

func (s *failingDiskStateStore) SaveDiskState(ctx context.Context, state domain.DiskState) error {
	if s.err != nil {
		return s.err
	}
	return s.Store.SaveDiskState(ctx, state)
}

func TestConnectionPersistenceFailureRetriesOriginalObservationTime(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.service.now = func() time.Time { return now }
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "A")
	h.system.detachUUID(uuidA)
	h.reconcile()
	now = now.Add(time.Hour)
	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))
	observed := now
	broken := &failingDiskStateStore{Store: h.store, err: errors.New("disk state commit failed")}
	h.service.store = broken
	if err := h.service.Reconcile(context.Background()); !errors.Is(err, broken.err) {
		t.Fatalf("failed persistence reported reconciliation success: %v", err)
	}
	broken.err = nil
	now = now.Add(time.Hour)
	h.reconcile()
	assertConnection(t, h.disk(id), observed)
}
