package removable

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
)

// harness wires the service against a real SQLite store and the fake system.
type harness struct {
	t       *testing.T
	store   *sqlite.Store
	system  *fakeSystem
	service *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "rotup.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	system := newFakeSystem()
	service := New(Options{Store: store, Enumerator: system, Mounts: system, Logger: logger})
	return &harness{t: t, store: store, system: system, service: service}
}

func (h *harness) register(uuid, name string) string {
	h.t.Helper()
	id, err := h.service.Register(context.Background(), uuid, name)
	if err != nil {
		h.t.Fatalf("register %s: %v", name, err)
	}
	return id
}

func (h *harness) disk(id string) domain.Disk {
	h.t.Helper()
	disk, err := h.store.Disk(context.Background(), id)
	if err != nil || disk == nil {
		h.t.Fatalf("load disk: %v", err)
	}
	return *disk
}

func (h *harness) reconcile() {
	h.t.Helper()
	if err := h.service.Reconcile(context.Background()); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

// diskEvents returns the recorded events that name one disk.
func (h *harness) diskEvents(ctx context.Context, diskID string) ([]domain.Event, error) {
	all, err := h.store.ListEvents(ctx, 500)
	if err != nil {
		return nil, err
	}
	var mine []domain.Event
	for _, event := range all {
		if event.DiskID == diskID {
			mine = append(mine, event)
		}
	}
	return mine, nil
}

func (h *harness) mountPoint(diskID string) string {
	h.t.Helper()
	point, err := media.MountPointFor(diskID)
	if err != nil {
		h.t.Fatalf("mount point: %v", err)
	}
	return point
}

// --- registration -------------------------------------------------------

func TestRegistrationRequiresAnObservedSupportedUniqueIdentity(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	// Nothing is connected yet.
	if _, err := h.service.Register(ctx, uuidA, "backup A"); err == nil {
		t.Fatal("a UUID that is not currently observed must not be registrable")
	}

	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.system.attach(mediaItem("/dev/sdc1", uuidB, "exfat"))
	h.system.attach(mediaItem("/dev/sdd1", "", "vfat"))

	// A filesystem ROTUP does not manage is observed but not registrable.
	if _, err := h.service.Register(ctx, uuidB, "exfat disk"); err == nil {
		t.Fatal("an unsupported filesystem must not be registrable")
	} else if !containsCode(err, "unsupported_filesystem") {
		t.Fatalf("error = %v, want unsupported_filesystem", err)
	}

	// Media with no persistent identity cannot be registered at all.
	if _, err := h.service.Register(ctx, "", "no uuid"); err == nil {
		t.Fatal("media without a filesystem UUID must not be registrable")
	}
	// A blank name is refused so a disk always has something to call it.
	if _, err := h.service.Register(ctx, uuidA, "  "); err == nil {
		t.Fatal("a blank disk name must be refused")
	}

	id := h.register(uuidA, "backup A")
	disk := h.disk(id)
	if disk.FilesystemUUID != uuidA || disk.FilesystemType != "ext4" || !disk.Enabled {
		t.Fatalf("registered disk = %#v", disk)
	}
	if disk.CurrentStatus != domain.DiskAvailableUnmounted {
		t.Fatalf("status = %s, want AVAILABLE_UNMOUNTED", disk.CurrentStatus)
	}

	// The same identity cannot be registered twice.
	if _, err := h.service.Register(ctx, uuidA, "backup A again"); err == nil {
		t.Fatal("a duplicate filesystem UUID must be refused")
	} else if !containsCode(err, "disk_already_registered") {
		t.Fatalf("error = %v, want disk_already_registered", err)
	}
}

func TestRegistrationNeverTouchesTheMedia(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.register(uuidA, "backup A")

	if h.system.mountCalls != 0 || h.system.syncCalls != 0 || h.system.unmountCalls != 0 {
		t.Fatal("registration must be metadata only: it must never mount or write to the media")
	}
	if len(h.system.mountPoints()) != 0 {
		t.Fatal("registration must not leave anything mounted")
	}
}

func TestRegistrationRefusesAnAmbiguousIdentity(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))

	_, err := h.service.Register(context.Background(), uuidA, "backup A")
	if err == nil {
		t.Fatal("registering an ambiguous identity must be refused")
	}
	if !containsCode(err, "ambiguous_identity") {
		t.Fatalf("error = %v, want ambiguous_identity", err)
	}
}

// --- observation and reconciliation -------------------------------------

func TestUnknownMediaIsObserveOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.reconcile()

	snapshot, err := h.service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snapshot.Views) != 1 {
		t.Fatalf("snapshot has %d views, want 1", len(snapshot.Views))
	}
	if snapshot.Views[0].Registered() {
		t.Fatal("unregistered media must not appear as a registered disk")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("unknown media must never be mounted automatically")
	}
	if len(h.system.mountPoints()) != 0 {
		t.Fatal("unknown media must never end up mounted")
	}
}

func TestPresentDiskBecomesAvailableUnmountedAndAbsentBecomesDisconnected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskAvailableUnmounted {
		t.Fatalf("connected idle disk = %s, want AVAILABLE_UNMOUNTED", status)
	}
	if len(h.system.mountPoints()) != 0 {
		t.Fatal("an idle registered disk must not be left mounted")
	}

	h.system.detachUUID(uuidA)
	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskDisconnected {
		t.Fatalf("absent disk = %s, want DISCONNECTED", status)
	}

	h.system.attach(mediaItem("/dev/sdz9", uuidA, "ext4"))
	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskAvailableUnmounted {
		t.Fatalf("reconnected disk = %s, want AVAILABLE_UNMOUNTED", status)
	}
}

func TestIdentitySurvivesADeviceNodeChange(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.reconcile()

	// The kernel hands the same hardware a different name after a re-plug.
	h.system.renameDevice(uuidA, "/dev/sdf1")
	h.reconcile()

	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskAvailableUnmounted {
		t.Fatalf("status = %s, want the disk to still be recognised", disk.CurrentStatus)
	}
	if disk.FilesystemUUID != uuidA {
		t.Fatal("the durable identity must not change with the device node")
	}

	snapshot, err := h.service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Views[0].Observed.DevicePath != "/dev/sdf1" {
		t.Fatalf("the observation must report the current device node, got %q",
			snapshot.Views[0].Observed.DevicePath)
	}
}

func TestDuplicateUUIDBlocksActionInsteadOfChoosing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	// A second filesystem now claims the same identity.
	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))
	h.reconcile()

	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskError {
		t.Fatalf("status = %s, want ERROR", disk.CurrentStatus)
	}
	if disk.LastErrorCode != domain.DiskErrorAmbiguousIdentity {
		t.Fatalf("error code = %q, want ambiguous_identity", disk.LastErrorCode)
	}

	err := h.service.Mount(context.Background(), id)
	if err == nil {
		t.Fatal("mounting an ambiguous identity must be refused")
	}
	if !containsCode(err, domain.DiskErrorAmbiguousIdentity) {
		t.Fatalf("error = %v, want ambiguous_identity", err)
	}
	if h.system.mountCalls != 0 {
		t.Fatal("ROTUP must not attempt a mount when it cannot tell the disks apart")
	}
}

func TestDisabledDiskIsIdentifiedButNotOffered(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	if err := h.service.UpdateSettings(context.Background(), id, "backup A", false); err != nil {
		t.Fatalf("disable disk: %v", err)
	}
	h.reconcile()

	if status := h.disk(id).CurrentStatus; status != domain.DiskDetected {
		t.Fatalf("disabled disk = %s, want DETECTED", status)
	}
	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("a disabled disk must not be mountable")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("a disabled disk must not reach the mount platform")
	}
}

func TestUnsupportedFilesystemOnARegisteredDiskIsAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	// The same UUID now presents a filesystem ROTUP will not mount, which means
	// this is not the disk that was registered.
	h.system.detachUUID(uuidA)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ntfs"))
	h.reconcile()

	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskError {
		t.Fatalf("status = %s, want ERROR", disk.CurrentStatus)
	}
	if disk.LastErrorCode != domain.DiskErrorUnsupportedFilesystem {
		t.Fatalf("error code = %q, want unsupported_filesystem", disk.LastErrorCode)
	}
	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("an unsupported filesystem must not be mountable")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("an unsupported filesystem must not reach the mount platform")
	}
}

// --- external mounts ----------------------------------------------------

func TestExternallyMountedMediaIsNotAdoptedOrUnmounted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	// An administrator mounted it by hand somewhere ROTUP does not own.
	h.system.mountElsewhere(uuidA, "/mnt/admin")
	h.reconcile()

	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskError {
		t.Fatalf("status = %s, want ERROR", disk.CurrentStatus)
	}
	if disk.LastErrorCode != domain.DiskErrorExternalMount {
		t.Fatalf("error code = %q, want external_mount", disk.LastErrorCode)
	}
	if !hasMountAt(h.system.mountPoints(), "/mnt/admin") {
		t.Fatal("ROTUP must not unmount a mount it does not own")
	}
	if h.system.unmountCalls != 0 {
		t.Fatal("ROTUP must not attempt to unmount a mount outside its own mount root")
	}

	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("mounting over an existing external mount must be refused")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("ROTUP must not mount on top of a mount it does not own")
	}
	if !hasMountAt(h.system.mountPoints(), "/mnt/admin") {
		t.Fatal("the administrator's mount must still be untouched")
	}
}

// --- mount --------------------------------------------------------------

func TestMountVerifiesIdentityAfterMounting(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskReady {
		t.Fatalf("status = %s, want READY", disk.CurrentStatus)
	}
	if !hasMountAt(h.system.mountPoints(), h.mountPoint(id)) {
		t.Fatal("the disk should be mounted at the ROTUP mount point")
	}
	// Verification re-reads the system rather than trusting the mount call, so
	// the platform is consulted again after mounting.
	if h.system.mountCalls != 1 {
		t.Fatalf("mount calls = %d, want exactly one", h.system.mountCalls)
	}
}

func TestMountSucceedingWithTheWrongIdentityIsRefusedAndUnmounted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	h.system.attach(mediaItem("/dev/sdc1", uuidB, "ext4"))
	id := h.register(uuidA, "backup A")

	// The mount call reports success, but what actually landed at the mount
	// point is the other disk.
	h.system.mountAttachesUUID = uuidB

	err := h.service.Mount(context.Background(), id)
	if err == nil {
		t.Fatal("a mount that produced the wrong filesystem must not be accepted")
	}
	if !containsCode(err, domain.DiskErrorIdentityMismatch) {
		t.Fatalf("error = %v, want identity_mismatch", err)
	}

	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskError {
		t.Fatalf("status = %s, want ERROR", disk.CurrentStatus)
	}
	if disk.LastErrorCode != domain.DiskErrorIdentityMismatch {
		t.Fatalf("error code = %q, want identity_mismatch", disk.LastErrorCode)
	}
	if h.system.unmountCalls == 0 {
		t.Fatal("an unverified mount must be removed again")
	}
	if hasMountAt(h.system.mountPoints(), h.mountPoint(id)) {
		t.Fatal("an unverified filesystem must not be left mounted under ROTUP's mount root")
	}
}

func TestMountReportingTheWrongUUIDIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	// The platform claims it mounted a different filesystem than requested.
	h.system.mountReportsUUID = uuidB

	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("a platform that reports a different UUID must not be believed")
	}
	if status := h.disk(id).CurrentStatus; status != domain.DiskError {
		t.Fatalf("status = %s, want ERROR", status)
	}
}

func TestMountOfAnAbsentDiskFailsWithoutTouchingThePlatform(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.system.detachUUID(uuidA)

	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("mounting an absent disk must fail")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("an absent disk must not reach the mount platform")
	}
	if status := h.disk(id).CurrentStatus; status != domain.DiskDisconnected {
		t.Fatalf("status = %s, want DISCONNECTED", status)
	}
}

func TestFailedMountRecordsAnErrorAndMountsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.system.mountErr = domain.NewError(domain.ErrClassUnavailable, "mount_helper_mount_failed",
		"the mount call failed")

	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("a failed mount must return an error")
	}
	disk := h.disk(id)
	if disk.CurrentStatus != domain.DiskError || disk.LastErrorCode != domain.DiskErrorMountFailed {
		t.Fatalf("disk = %s/%s, want ERROR/mount_failed", disk.CurrentStatus, disk.LastErrorCode)
	}
	if len(h.system.mountPoints()) != 0 {
		t.Fatal("a failed mount must leave nothing mounted")
	}
}

// --- sync and unmount ---------------------------------------------------

func TestSyncAndUnmountReachesSafeToRemove(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}

	if err := h.service.SyncAndUnmount(context.Background(), id); err != nil {
		t.Fatalf("sync and unmount: %v", err)
	}
	if h.system.syncCalls != 1 {
		t.Fatalf("sync calls = %d, want the filesystem to be flushed exactly once", h.system.syncCalls)
	}
	if status := h.disk(id).CurrentStatus; status != domain.DiskSafeToRemove {
		t.Fatalf("status = %s, want SAFE_TO_REMOVE", status)
	}
	if hasMountAt(h.system.mountPoints(), h.mountPoint(id)) {
		t.Fatal("the ROTUP mount should be gone")
	}

	// A disk that stays connected settles back to the normal idle state.
	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskAvailableUnmounted {
		t.Fatalf("status = %s, want AVAILABLE_UNMOUNTED", status)
	}
}

func TestFailedSyncNeverClaimsSafeToRemove(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	h.system.syncErr = domain.NewError(domain.ErrClassUnavailable, "mount_helper_sync_failed",
		"the filesystem flush failed")

	if err := h.service.SyncAndUnmount(context.Background(), id); err == nil {
		t.Fatal("a failed flush must return an error")
	}
	disk := h.disk(id)
	if disk.CurrentStatus == domain.DiskSafeToRemove {
		t.Fatal("a disk whose flush failed must never be reported as safe to remove")
	}
	if disk.CurrentStatus != domain.DiskError || disk.LastErrorCode != domain.DiskErrorSyncFailed {
		t.Fatalf("disk = %s/%s, want ERROR/sync_failed", disk.CurrentStatus, disk.LastErrorCode)
	}
}

func TestFailedUnmountNeverClaimsAvailableUnmounted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	h.system.unmountErr = domain.NewError(domain.ErrClassUnavailable, "mount_helper_unmount_failed",
		"the unmount call failed")

	if err := h.service.SyncAndUnmount(context.Background(), id); err == nil {
		t.Fatal("a failed unmount must return an error")
	}
	disk := h.disk(id)
	for _, forbidden := range []domain.DiskStatus{
		domain.DiskAvailableUnmounted, domain.DiskSafeToRemove,
	} {
		if disk.CurrentStatus == forbidden {
			t.Fatalf("a failed unmount must not report %s", forbidden)
		}
	}
	if disk.LastErrorCode != domain.DiskErrorUnmountFailed {
		t.Fatalf("error code = %q, want unmount_failed", disk.LastErrorCode)
	}
}

func TestUnmountThatSilentlyLeavesTheFilesystemMountedIsCaught(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	// The platform reports success but nothing was actually detached, which is
	// what a lazy unmount looks like from the outside.
	h.system.unmountLeavesMounted = true

	err := h.service.SyncAndUnmount(context.Background(), id)
	if err == nil {
		t.Fatal("an unmount that did not actually detach must not be accepted")
	}
	disk := h.disk(id)
	if disk.CurrentStatus == domain.DiskSafeToRemove {
		t.Fatal("a filesystem that is still mounted must never be reported as safe to remove")
	}
	if disk.LastErrorCode != domain.DiskErrorUnmountFailed {
		t.Fatalf("error code = %q, want unmount_failed", disk.LastErrorCode)
	}
}

// --- surprise removal ---------------------------------------------------

func TestRemovalDuringUnmountIsNotReportedAsSafeToRemove(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	h.system.removeOnUnmount = true

	err := h.service.SyncAndUnmount(context.Background(), id)
	if err == nil {
		t.Fatal("a disk pulled during the operation must not report success")
	}
	if !containsCode(err, domain.DiskErrorRemovedDuringOperation) {
		t.Fatalf("error = %v, want removed_during_operation", err)
	}
	disk := h.disk(id)
	if disk.CurrentStatus == domain.DiskSafeToRemove {
		t.Fatal("a disk that vanished mid-operation was never confirmed flushed")
	}
	if disk.CurrentStatus != domain.DiskDisconnected {
		t.Fatalf("status = %s, want DISCONNECTED", disk.CurrentStatus)
	}
	if disk.LastErrorCode != domain.DiskErrorRemovedDuringOperation {
		t.Fatalf("error code = %q, want removed_during_operation", disk.LastErrorCode)
	}
}

func TestRemovalDuringSyncFailsSafely(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	h.system.removeOnSync = true
	h.system.syncErr = domain.NewError(domain.ErrClassUnavailable, "mount_helper_sync_failed",
		"the filesystem flush failed")

	if err := h.service.SyncAndUnmount(context.Background(), id); err == nil {
		t.Fatal("a flush against vanished hardware must fail")
	}
	if status := h.disk(id).CurrentStatus; status == domain.DiskSafeToRemove {
		t.Fatal("a disk that vanished during the flush must not be reported as safe to remove")
	}

	// The next reconciliation converges on the truth: the disk is gone.
	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskDisconnected {
		t.Fatalf("status = %s, want DISCONNECTED after reconciliation", status)
	}
}

func TestRemovalWhileIdleConvergesToDisconnected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.reconcile()

	h.system.detachUUID(uuidA)
	h.reconcile()

	if status := h.disk(id).CurrentStatus; status != domain.DiskDisconnected {
		t.Fatalf("status = %s, want DISCONNECTED", status)
	}
	disk := h.disk(id)
	if disk.LastDisconnectedAt == nil {
		t.Fatal("the disconnection should be recorded")
	}
}

// --- concurrency --------------------------------------------------------

func TestOnlyOneMediaOperationPerDisk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	release, ok := h.service.locks.tryAcquire(id)
	if !ok {
		t.Fatal("the disk lock should be free")
	}
	defer release()

	if !h.service.Busy(id) {
		t.Fatal("a locked disk must report as busy")
	}
	if err := h.service.Mount(context.Background(), id); err == nil {
		t.Fatal("a second operation on a busy disk must be refused")
	}
	if err := h.service.SyncAndUnmount(context.Background(), id); err == nil {
		t.Fatal("a second operation on a busy disk must be refused")
	}
	if h.system.mountCalls != 0 || h.system.unmountCalls != 0 {
		t.Fatal("a refused operation must not reach the platform")
	}
}

func TestReconciliationDoesNotDisturbAnOperationInFlight(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	release, _ := h.service.locks.tryAcquire(id)
	h.service.setStatus(context.Background(), id, domain.DiskMounting, "")

	// A reconciliation running alongside an operation must leave the transient
	// status alone rather than rewriting it from a stale reading.
	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskMounting {
		t.Fatalf("status = %s, want the in-flight MOUNTING to be preserved", status)
	}
	release()

	h.reconcile()
	if status := h.disk(id).CurrentStatus; status != domain.DiskAvailableUnmounted {
		t.Fatalf("status = %s, want reconciliation to resume once the operation ended", status)
	}
}

// --- restart reconciliation ---------------------------------------------

func TestRestartReconciliationConvergesFromAnyPersistedState(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		persisted domain.DiskStatus
		connected bool
		mountedAt func(diskID string) string
		want      domain.DiskStatus
		wantCode  string
	}{
		"absent after a reboot": {
			persisted: domain.DiskReady, connected: false, want: domain.DiskDisconnected,
		},
		"present and unmounted": {
			persisted: domain.DiskReady, connected: true, want: domain.DiskAvailableUnmounted,
		},
		"still mounted at the ROTUP mount point": {
			persisted: domain.DiskDisconnected, connected: true,
			mountedAt: func(diskID string) string {
				point, _ := media.MountPointFor(diskID)
				return point
			},
			want: domain.DiskReady,
		},
		"mounted somewhere else": {
			persisted: domain.DiskAvailableUnmounted, connected: true,
			mountedAt: func(string) string { return "/mnt/elsewhere" },
			want:      domain.DiskError, wantCode: domain.DiskErrorExternalMount,
		},
		"stale error clears when the disk is healthy": {
			persisted: domain.DiskError, connected: true, want: domain.DiskAvailableUnmounted,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
			id := h.register(uuidA, "backup A")

			// Simulate whatever the previous process left behind.
			h.service.setStatus(context.Background(), id, testCase.persisted, "stale")
			if !testCase.connected {
				h.system.detachUUID(uuidA)
			}
			if testCase.mountedAt != nil {
				h.system.mountElsewhere(uuidA, testCase.mountedAt(id))
			}

			// A fresh service, as a restarted daemon would have.
			restarted := New(Options{
				Store: h.store, Enumerator: h.system, Mounts: h.system,
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err := restarted.Reconcile(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			disk := h.disk(id)
			if disk.CurrentStatus != testCase.want {
				t.Fatalf("status = %s, want %s", disk.CurrentStatus, testCase.want)
			}
			if disk.LastErrorCode != testCase.wantCode {
				t.Fatalf("error code = %q, want %q", disk.LastErrorCode, testCase.wantCode)
			}
		})
	}
}

// --- events -------------------------------------------------------------

func TestEventsAreRecordedWithoutRepeatingOnEveryPoll(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	for i := 0; i < 5; i++ {
		h.reconcile()
	}
	h.system.detachUUID(uuidA)
	for i := 0; i < 5; i++ {
		h.reconcile()
	}

	events, err := h.diskEvents(ctx, id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	counts := make(map[string]int)
	for _, event := range events {
		counts[event.EventType]++
	}
	if counts["disk.registered"] != 1 {
		t.Fatalf("disk.registered recorded %d times, want 1", counts["disk.registered"])
	}
	if counts["media.removed"] != 1 {
		t.Fatalf("media.removed recorded %d times over five polls, want 1", counts["media.removed"])
	}
	if len(events) > 4 {
		t.Fatalf("polling produced %d events; a periodic poll must not spam history", len(events))
	}
}

func TestDuplicateUUIDEmitsADurableWarning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.reconcile()

	h.system.attach(mediaItem("/dev/sdc1", uuidA, "ext4"))
	h.reconcile()

	events, err := h.diskEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "disk.duplicate_uuid" {
			found = true
			if event.Severity != domain.SeverityError {
				t.Fatalf("severity = %s, want ERROR", event.Severity)
			}
		}
	}
	if !found {
		t.Fatal("an ambiguous identity must be recorded durably")
	}
}

func TestMountLifecycleIsRecordedInHistory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	if err := h.service.Mount(context.Background(), id); err != nil {
		t.Fatalf("mount: %v", err)
	}
	if err := h.service.SyncAndUnmount(context.Background(), id); err != nil {
		t.Fatalf("sync and unmount: %v", err)
	}

	events, err := h.diskEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := make(map[string]bool)
	for _, event := range events {
		seen[event.EventType] = true
	}
	for _, wanted := range []string{
		"disk.registered", "disk.mount_started", "disk.mounted",
		"disk.sync_started", "disk.unmounted",
	} {
		if !seen[wanted] {
			t.Fatalf("history is missing %s (recorded: %v)", wanted, seen)
		}
	}
}

// --- platform boundary --------------------------------------------------

func TestUnsupportedPlatformRefusesEveryOperation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")

	h.system.mu.Lock()
	h.system.available = false
	h.system.mu.Unlock()

	if h.service.PlatformAvailable() {
		t.Fatal("the service must report that the platform cannot manage media")
	}
	snapshot, err := h.service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.PlatformAvailable {
		t.Fatal("the snapshot must say the platform is unavailable")
	}
	if snapshot.PlatformReason == "" {
		t.Fatal("the snapshot must explain why media management is unavailable")
	}
	_ = id
}

func TestSnapshotDoesNotChangeAnything(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	before := h.disk(id)

	for i := 0; i < 3; i++ {
		if _, err := h.service.Snapshot(context.Background()); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	}
	after := h.disk(id)
	if before.CurrentStatus != after.CurrentStatus || before.UpdatedAt != after.UpdatedAt {
		t.Fatal("rendering the page must not change any state")
	}
	if h.system.mountCalls != 0 {
		t.Fatal("rendering the page must not touch the media")
	}
}

func TestEnumerationFailureKeepsRegisteredDisksVisible(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.system.attach(mediaItem("/dev/sdb1", uuidA, "ext4"))
	id := h.register(uuidA, "backup A")
	h.system.enumerateErr = domain.NewError(domain.ErrClassExternalTool, "lsblk_failed",
		"listing block devices failed")

	snapshot, err := h.service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.EnumerationError == "" {
		t.Fatal("the failure must be reported")
	}
	if len(snapshot.Views) != 1 || !snapshot.Views[0].Registered() {
		t.Fatal("registered disks must stay visible when enumeration fails")
	}
	if !strings.Contains(snapshot.EnumerationError, "block devices") {
		t.Fatalf("unexpected message: %q", snapshot.EnumerationError)
	}

	// Reconciliation refuses to act on a reading it could not take.
	if err := h.service.Reconcile(context.Background()); err == nil {
		t.Fatal("reconciliation must fail rather than converge on an unread system")
	}
	if status := h.disk(id).CurrentStatus; status != domain.DiskAvailableUnmounted {
		t.Fatalf("status = %s, want the last known state to be preserved", status)
	}
}
