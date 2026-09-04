//go:build linux

package linux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/platform/process"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/removable"
)

// This is the M3 Linux mechanism spike. It proves on a real kernel what the
// portable tests can only assume: that lsblk reports what ROTUP parses, that
// the privileged helper mounts, flushes and unmounts an ext4 filesystem, that
// identity is resolved by UUID rather than by device node, and that a wrong
// identity is refused.
//
// It is opt-in and requires root, because creating loop devices and mounting
// filesystems does. Run it with:
//
//	sudo ROTUP_LINUX_SPIKE=1 \
//	  ROTUP_MOUNT_HELPER=/path/to/rotup-mounthelper \
//	  go test ./internal/platform/linux/ -run Spike -v
//
// Everything destructive happens inside a filesystem image this test created in
// its own temporary directory and attached to a loop device it allocated. No
// pre-existing host disk, partition, or mount is ever touched.
const spikeEnv = "ROTUP_LINUX_SPIKE"

func requireSpike(t *testing.T) {
	t.Helper()
	if os.Getenv(spikeEnv) == "" {
		t.Skipf("set %s=1 and run as root to exercise the Linux media mechanism", spikeEnv)
	}
	if os.Geteuid() != 0 {
		t.Fatalf("%s is set but the test is not running as root; loop devices and mounts need it", spikeEnv)
	}
	for _, tool := range []string{"losetup", "mkfs.ext4", "lsblk", "blkid"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required for the spike and was not found: %v", tool, err)
		}
	}
}

func helperPath(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("ROTUP_MOUNT_HELPER"))
	if path == "" {
		t.Fatal("set ROTUP_MOUNT_HELPER to the built rotup-mounthelper binary")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the configured mount helper is not usable: %v", err)
	}
	return path
}

// loopImage is a filesystem image attached to a loop device, created and
// destroyed entirely within one test.
type loopImage struct {
	t      *testing.T
	path   string
	device string
	uuid   string
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// newLoopImage creates an ext4 image and attaches it to a loop device.
func newLoopImage(t *testing.T, sizeMB int) *loopImage {
	t.Helper()

	imagePath := filepath.Join(t.TempDir(), fmt.Sprintf("rotup-spike-%d.img", time.Now().UnixNano()))
	handle, err := os.Create(imagePath)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := handle.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		t.Fatalf("size image: %v", err)
	}
	_ = handle.Close()

	// mkfs is pointed at the image file, never at a device.
	run(t, "mkfs.ext4", "-q", "-F", imagePath)

	device := run(t, "losetup", "--find", "--show", imagePath)
	image := &loopImage{t: t, path: imagePath, device: device}
	t.Cleanup(image.detach)

	image.uuid = strings.ToLower(run(t, "blkid", "--probe", "-s", "UUID", "-o", "value", device))
	if image.uuid == "" {
		t.Fatal("the test filesystem reported no UUID")
	}
	// Device creation and udev publication are asynchronous on Linux. Wait for
	// the fixture's known, directly-probed UUID, not for a by-uuid symlink.
	waitForFilesystem(t, device, image.uuid)
	return image
}

func (i *loopImage) detach() {
	if i.device == "" {
		return
	}
	_ = exec.Command("umount", i.device).Run()
	_ = exec.Command("losetup", "--detach", i.device).Run()
	i.device = ""
}

// reattach allocates the second node while the first is still occupied. The
// image is unmounted and neither mapping is written through. Only after the
// second attachment exists is the first released, making the node change
// deterministic even when the kernel always chooses the lowest free number.
func (i *loopImage) reattach() *loopImage {
	i.t.Helper()
	other := &loopImage{t: i.t, path: i.path, uuid: i.uuid,
		device: run(i.t, "losetup", "--find", "--show", i.path)}
	i.t.Cleanup(other.detach)
	if other.device == i.device {
		i.t.Fatal("the second simultaneous attachment must use a different device node")
	}
	run(i.t, "losetup", "--detach", i.device)
	waitForLoopDetach(i.t, i.device)
	i.device = ""
	waitForFilesystem(i.t, other.device, other.uuid)
	return other
}

func waitForFilesystem(t *testing.T, device, uuid string) {
	t.Helper()
	enumerator := spikeEnumerator(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		item, err := enumerator.ResolveFilesystem(context.Background(), uuid)
		if err == nil && item.DevicePath == device {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture %s with UUID %s was not published: item=%#v error=%v", device, uuid, item, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func loopStatus(t *testing.T, device string) *unix.LoopInfo64 {
	t.Helper()
	handle, err := os.Open(device)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("open loop device: %v", err)
	}
	defer handle.Close()
	info, err := unix.IoctlLoopGetStatus64(int(handle.Fd()))
	if errors.Is(err, unix.ENXIO) {
		return nil // Kernel confirms that no backing file is attached.
	}
	if err != nil {
		t.Fatalf("read kernel loop status: %v", err)
	}
	return info
}

func waitForLoopDetach(t *testing.T, device string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for loopStatus(t, device) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s still has an attached backing file", device)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func spikeEnumerator(t *testing.T) *Enumerator {
	t.Helper()
	path, err := exec.LookPath("lsblk")
	if err != nil {
		t.Fatalf("lsblk: %v", err)
	}
	return NewEnumerator(path, process.NewRunner())
}

func findByUUID(observed []domain.ObservedMedia, uuid string) []domain.ObservedMedia {
	var matches []domain.ObservedMedia
	for _, item := range observed {
		if item.FilesystemUUID == uuid {
			matches = append(matches, item)
		}
	}
	return matches
}

// A. Enumerate a test filesystem and obtain its UUID.
func TestSpikeEnumerationFindsTheTestFilesystem(t *testing.T) {
	requireSpike(t)
	image := newLoopImage(t, 64)

	observed, err := spikeEnumerator(t).Enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	matches := findByUUID(observed, image.uuid)
	if len(matches) != 1 {
		t.Fatalf("found %d filesystems with UUID %s, want exactly 1", len(matches), image.uuid)
	}
	if matches[0].FilesystemType != "ext4" {
		t.Fatalf("filesystem type = %q, want ext4", matches[0].FilesystemType)
	}
	if matches[0].DevicePath != image.device {
		t.Fatalf("device path = %q, want %q", matches[0].DevicePath, image.device)
	}
	t.Logf("SPIKE: enumerated %s as %s (%s)", image.uuid, matches[0].DevicePath, matches[0].FilesystemType)
}

// B. The same UUID is resolved independently of any particular device node.
func TestSpikeIdentitySurvivesADeviceNodeChange(t *testing.T) {
	requireSpike(t)
	image := newLoopImage(t, 64)
	enumerator := spikeEnumerator(t)

	before, err := enumerator.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	initial := findByUUID(before, image.uuid)
	if len(initial) != 1 {
		t.Fatalf("initial UUID matches = %d, want 1", len(initial))
	}
	firstDevice := initial[0].DevicePath
	image = image.reattach()
	secondDevice := image.device
	if firstDevice == secondDevice {
		t.Fatal("the spike did not exercise a device-node change")
	}

	after, err := enumerator.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	matches := findByUUID(after, image.uuid)
	if len(matches) != 1 {
		t.Fatalf("found %d filesystems with the UUID after re-attaching, want 1", len(matches))
	}
	if matches[0].DevicePath != secondDevice {
		t.Fatalf("device path = %q, want the current node %q", matches[0].DevicePath, secondDevice)
	}
	// Prove that the privileged path re-resolves too, not just discovery.
	manager := NewMountManager(helperPath(t), process.NewRunner())
	diskID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Unmount(context.Background(), diskID) })
	result, err := manager.Mount(context.Background(), ports.MountRequest{
		DiskID: diskID, FilesystemUUID: image.uuid, FilesystemType: "ext4",
	})
	if err != nil || !result.Mounted || result.ResolvedDevice != secondDevice || result.FilesystemUUID != image.uuid {
		t.Fatalf("mount after node change = %#v, %v", result, err)
	}
	if err := manager.Sync(context.Background(), diskID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unmount(context.Background(), diskID); err != nil {
		t.Fatal(err)
	}
	t.Logf("SPIKE: UUID %s resolved across a device node change from %s to %s",
		image.uuid, firstDevice, secondDevice)
}

// C and D. Mount, verify the mounted identity, write, sync, unmount, then
// re-mount independently and confirm the data is there.
func TestSpikeMountVerifySyncUnmountRoundTrip(t *testing.T) {
	requireSpike(t)
	image := newLoopImage(t, 64)
	manager := NewMountManager(helperPath(t), process.NewRunner())
	enumerator := spikeEnumerator(t)
	ctx := context.Background()

	diskID, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	expected, err := media.MountPointFor(diskID)
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	t.Cleanup(func() { _ = manager.Unmount(context.Background(), diskID) })

	result, err := manager.Mount(ctx, ports.MountRequest{
		DiskID: diskID, FilesystemUUID: image.uuid, FilesystemType: "ext4",
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if result.MountPoint != expected {
		t.Fatalf("mount point = %q, want the derived %q", result.MountPoint, expected)
	}
	if result.FilesystemUUID != image.uuid || result.FilesystemType != "ext4" || !result.Mounted {
		t.Fatalf("the helper did not verify the mounted identity: %#v", result)
	}

	// The mount is only believed after the kernel mount table and the block
	// device list agree that the expected UUID is what landed there.
	mounts, err := manager.Observe(ctx)
	if err != nil {
		t.Fatalf("observe mounts: %v", err)
	}
	var mounted *domain.MountObservation
	for i := range mounts {
		if mounts[i].MountPoint == expected {
			mounted = &mounts[i]
		}
	}
	if mounted == nil {
		t.Fatalf("nothing is mounted at %s", expected)
	}
	observed, err := enumerator.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	verified := false
	for _, item := range observed {
		if item.DevicePath == mounted.Source && item.FilesystemUUID == image.uuid {
			verified = true
		}
	}
	if !verified {
		t.Fatalf("the filesystem mounted at %s does not resolve to UUID %s", expected, image.uuid)
	}

	// The mount options the helper pins must actually be in force.
	if !strings.Contains(readMountOptions(t, expected), "noexec") {
		t.Error("the ROTUP mount should be noexec")
	}

	payload := filepath.Join(expected, "rotup-spike.txt")
	if err := os.WriteFile(payload, []byte("written by the M3 spike"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	if err := manager.Sync(ctx, diskID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := manager.Unmount(ctx, diskID); err != nil {
		t.Fatalf("unmount: %v", err)
	}

	mounts, err = manager.Observe(ctx)
	if err != nil {
		t.Fatalf("observe mounts: %v", err)
	}
	for _, mount := range mounts {
		if mount.MountPoint == expected {
			t.Fatal("the filesystem is still mounted after unmounting")
		}
	}

	// Mount again from scratch and confirm the flushed data survived.
	if _, err := manager.Mount(ctx, ports.MountRequest{
		DiskID: diskID, FilesystemUUID: image.uuid, FilesystemType: "ext4",
	}); err != nil {
		t.Fatalf("re-mount: %v", err)
	}
	contents, err := os.ReadFile(payload)
	if err != nil {
		t.Fatalf("the flushed file is missing after re-mounting: %v", err)
	}
	if string(contents) != "written by the M3 spike" {
		t.Fatalf("file contents = %q", contents)
	}
	if err := manager.Unmount(ctx, diskID); err != nil {
		t.Fatalf("final unmount: %v", err)
	}
	t.Logf("SPIKE: mounted, verified, wrote, synced and unmounted %s at %s", image.uuid, expected)
}

// E. An operation naming an identity that is not present is refused.
func TestSpikeWrongIdentityIsRefused(t *testing.T) {
	requireSpike(t)
	manager := NewMountManager(helperPath(t), process.NewRunner())

	diskID, err := domain.NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	// A well-formed UUID that no connected filesystem carries.
	_, err = manager.Mount(context.Background(), ports.MountRequest{
		DiskID: diskID, FilesystemUUID: "deadbeef-dead-beef-dead-beefdeadbeef", FilesystemType: "ext4",
	})
	if err == nil {
		t.Fatal("mounting a UUID that is not present must be refused")
	}
	t.Logf("SPIKE: absent identity refused with %v", err)

	point, _ := media.MountPointFor(diskID)
	if _, statErr := os.Stat(point); statErr == nil {
		t.Fatal("a refused mount must not leave a mount point behind")
	}
}

func TestSpikeDuplicateIdentityIsRefusedBeforeMount(t *testing.T) {
	requireSpike(t)
	image := newLoopImage(t, 64)
	// Both mappings are unmounted and are never written through. They expose
	// the same UUID simultaneously, which must not authorize either mount.
	duplicate := &loopImage{t: t, path: image.path, uuid: image.uuid,
		device: run(t, "losetup", "--find", "--show", image.path)}
	t.Cleanup(duplicate.detach)
	enumerator := spikeEnumerator(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := enumerator.ResolveFilesystem(context.Background(), image.uuid)
		var typed *domain.Error
		if domain.AsError(err, &typed) && typed.Code == "ambiguous_identity" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("duplicate fixture never became observable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager := NewMountManager(helperPath(t), process.NewRunner())
	diskID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Unmount(context.Background(), diskID) })
	_, err = manager.Mount(context.Background(), ports.MountRequest{
		DiskID: diskID, FilesystemUUID: image.uuid, FilesystemType: "ext4",
	})
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Code != "mount_helper_ambiguous_identity" {
		t.Fatalf("ambiguous mount = %v, want mount_helper_ambiguous_identity", err)
	}
	point, _ := media.MountPointFor(diskID)
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Fatalf("ambiguous identity left a mount point: %v", err)
	}
}

func TestSpikeHelperRefusesUnprivilegedCaller(t *testing.T) {
	requireSpike(t)
	// A separately traversable directory lets the unprivileged child execute
	// the helper even when the Go test's own temporary parent is root-only.
	dir, err := os.MkdirTemp("", "rotup-helper-unprivileged-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(helperPath(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rotup-mounthelper")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "mount", "--disk-id", "11111111-2222-4333-8444-555555555555",
		"--fs-uuid", "deadbeef-dead-beef-dead-beefdeadbeef", "--fstype", "ext4")
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	output, err := cmd.Output()
	if err == nil {
		t.Fatal("unprivileged helper invocation unexpectedly succeeded")
	}
	var result media.Result
	if err := json.Unmarshal(output, &result); err != nil || result.OK || result.ErrorCode != "privilege_required" {
		t.Fatalf("unprivileged result = %s, parse error = %v", output, err)
	}
}

// The helper must refuse anything outside its narrow contract, even when it is
// invoked directly rather than through ROTUP.
func TestSpikeHelperRefusesUnsafeArguments(t *testing.T) {
	requireSpike(t)
	helper := helperPath(t)

	cases := [][]string{
		{"mount", "--disk-id", "../../etc", "--fs-uuid", "1111", "--fstype", "ext4"},
		{"mount", "--disk-id", "11111111-2222-4333-8444-555555555555", "--fs-uuid", "/dev/sda1", "--fstype", "ext4"},
		{"mount", "--disk-id", "11111111-2222-4333-8444-555555555555", "--fs-uuid", "1111", "--fstype", "ntfs"},
		{"format", "--disk-id", "11111111-2222-4333-8444-555555555555"},
		{"mount", "--disk-id", "11111111-2222-4333-8444-555555555555", "--fs-uuid", "1111", "--fstype", "ext4", "/etc"},
	}
	for _, args := range cases {
		output, err := exec.Command(helper, args...).Output()
		if err == nil {
			t.Fatalf("the helper accepted %v", args)
		}
		var result media.Result
		if json.Unmarshal(output, &result) == nil && result.OK {
			t.Fatalf("the helper reported success for %v", args)
		}
	}
	t.Log("SPIKE: the helper refused every unsafe argument shape")
}

// G. Linux defers loop detachment while a mount holds a reference. Exercise
// actual removal at the final unmount and require the real service (not just
// a helper exit code) to refuse SAFE_TO_REMOVE. A loop detach request cannot
// simulate physical USB loss during I/O; portable fault tests cover that path.
func TestSpikeSurpriseRemovalProducesNoFalseSuccess(t *testing.T) {
	requireSpike(t)
	image := newLoopImage(t, 64)
	manager := NewMountManager(helperPath(t), process.NewRunner())
	enumerator := spikeEnumerator(t)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "spike.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	service := removable.New(removable.Options{Store: store, Enumerator: enumerator, Mounts: manager, Logger: logger})
	diskID, err := service.Register(ctx, image.uuid, "spike removal")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := service.Mount(ctx, diskID); err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() { _ = manager.Unmount(context.Background(), diskID) })

	point, _ := manager.MountPointFor(diskID)
	if err := os.WriteFile(filepath.Join(point, "pending-flush"), []byte("M3 removal spike"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A successful detach request sets AUTOCLEAR, not physical disappearance.
	run(t, "losetup", "--detach", image.device)
	detached := image.device
	info := loopStatus(t, detached)
	if info == nil || info.Flags&unix.LO_FLAGS_AUTOCLEAR == 0 {
		t.Fatal("the mounted loop should still be attached with deferred AUTOCLEAR removal")
	}

	observed, err := enumerator.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(findByUUID(observed, image.uuid)) != 1 {
		t.Fatal("a backing filesystem still held by a live mount must remain observed")
	}
	// The final mount reference is released by SyncAndUnmount. The kernel now
	// really detaches the backing file; the service must not report a safe disk.
	unmountErr := service.SyncAndUnmount(ctx, diskID)
	var typed *domain.Error
	if !domain.AsError(unmountErr, &typed) || typed.Code != domain.DiskErrorRemovedDuringOperation {
		t.Fatalf("removal must not succeed: got %v, want removed_during_operation", unmountErr)
	}
	waitForLoopDetach(t, detached)
	image.device = ""
	disk, err := store.Disk(ctx, diskID)
	if err != nil || disk == nil {
		t.Fatalf("read disk: %v", err)
	}
	if disk.CurrentStatus != domain.DiskDisconnected || disk.LastErrorCode != domain.DiskErrorRemovedDuringOperation {
		t.Fatalf("removed media must be DISCONNECTED, never SAFE_TO_REMOVE: %#v", disk)
	}

	observed, err = enumerator.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(findByUUID(observed, image.uuid)) != 0 {
		t.Fatal("the removed filesystem must stay absent")
	}
	t.Logf("SPIKE: %s genuinely detached after the last reference closed; service refused safe removal: %v", detached, unmountErr)
}

// readMountOptions returns the super options recorded for a mount point.
func readMountOptions(t *testing.T, mountPoint string) string {
	t.Helper()
	raw, err := os.ReadFile(mountInfoPath)
	if err != nil {
		t.Fatalf("read mountinfo: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 5 && unescapeMountField(fields[4]) == mountPoint {
			return fields[5]
		}
	}
	return ""
}
