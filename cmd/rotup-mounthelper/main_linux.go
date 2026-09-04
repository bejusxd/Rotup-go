//go:build linux

// Command rotup-mounthelper performs the three privileged filesystem
// operations ROTUP needs — mount, sync, unmount — and nothing else.
//
// # Why a helper exists
//
// mount(2) requires CAP_SYS_ADMIN, which is effectively root. Giving that to
// the whole rotupd daemon would hand the same privilege to its HTTP server, its
// SQLite layer, and every external tool it spawns. Instead the privilege is
// confined to this binary, whose entire interface is three subcommands with
// three validated arguments.
//
// The alternative mechanisms were rejected for concrete reasons. udisks2 picks
// its own mount point under /run/media/<user>/<label>, which ROTUP cannot
// derive or verify, and it expects a D-Bus session and polkit policy that a
// headless unattended host should not need. Granting CAP_SYS_ADMIN to rotupd
// itself is a far wider grant than the problem requires.
//
// # What the helper will not do
//
// It accepts no path, no mount option, no device node, and no shell. It derives
// the mount point from the validated disk identifier, resolves the device from
// the filesystem UUID itself, and hard-codes the mount flags. There is no
// subcommand that can run anything else.
//
// # Installation
//
// The recommended production wiring is a narrow sudo rule:
//
//	rotup ALL=(root) NOPASSWD: /usr/libexec/rotup-mounthelper
//
// with the helper owned root:root mode 0755, invoked through a sudo wrapper.
// Alternatively install it setuid root with mode 4750 and group rotup, so only
// the service identity can execute it. Either way the helper's own validation
// is the security boundary, not the invocation mechanism.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/linux"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/platform/process"
)

// mountFlags are fixed. Backup media is data, never something to execute or to
// let define device nodes or setuid binaries.
const mountFlags = unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC

func main() {
	// The helper may run setuid, so the inherited environment is discarded
	// before anything else happens.
	os.Clearenv()

	if len(os.Args) < 2 {
		fail("usage", "expected one of: mount, sync, unmount")
	}
	operation := os.Args[1]

	set := flag.NewFlagSet(operation, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	diskID := set.String(strings.TrimPrefix(media.FlagDiskID, "--"), "", "ROTUP disk identifier")
	fsUUID := set.String(strings.TrimPrefix(media.FlagFSUUID, "--"), "", "filesystem UUID")
	fsType := set.String(strings.TrimPrefix(media.FlagFSType, "--"), "", "filesystem type")
	if err := set.Parse(os.Args[2:]); err != nil {
		fail("usage", "arguments could not be parsed")
	}
	if set.NArg() != 0 {
		fail("usage", "unexpected positional arguments")
	}

	if err := media.ValidateDiskID(*diskID); err != nil {
		fail("invalid_disk_id", err.Error())
	}
	mountPoint, err := media.MountPointFor(*diskID)
	if err != nil {
		fail("invalid_disk_id", err.Error())
	}

	switch operation {
	case media.CommandMount:
		if err := media.ValidateFilesystemUUID(*fsUUID); err != nil {
			fail("invalid_filesystem_uuid", err.Error())
		}
		if err := media.ValidateFilesystemType(*fsType); err != nil {
			fail("unsupported_filesystem", err.Error())
		}
		requirePrivilege()
		doMount(mountPoint, *fsUUID, *fsType)
	case media.CommandSync:
		requirePrivilege()
		doSync(mountPoint)
	case media.CommandUnmount:
		requirePrivilege()
		doUnmount(mountPoint)
	default:
		fail("usage", "unknown operation")
	}
}

func requirePrivilege() {
	if os.Geteuid() != 0 {
		fail("privilege_required", "the mount helper must run as root through the installed sudo wrapper or setuid helper; keep rotupd unprivileged")
	}
}

func doMount(mountPoint, fsUUID, fsType string) {
	normalizedUUID := domain.NormalizeFilesystemUUID(fsUUID)
	normalizedType := domain.NormalizeFilesystemType(fsType)
	ctx := context.Background()
	runner := process.NewRunner()
	// These are trusted installation paths, never caller input or PATH lookups.
	// Reuse the daemon's enumeration rules instead of relying on asynchronous
	// udev symlinks, which may not exist even when the filesystem is readable.
	enumerator := linux.NewEnumerator("/usr/bin/lsblk", runner)
	observed, err := enumerator.ResolveFilesystem(ctx, normalizedUUID)
	if err != nil {
		code := "identity_resolution_failed"
		var typed *domain.Error
		if domain.AsError(err, &typed) {
			code = typed.Code
		}
		fail(code, "filesystem identity could not be resolved: "+err.Error())
	}
	device := observed.DevicePath
	if !strings.HasPrefix(device, "/dev/") || filepath.Clean(device) != device {
		fail("device_not_found", "the UUID did not resolve to a canonical device node")
	}
	// Hold the selected block device open across the mount and probe its actual
	// signature without a blkid cache, before AND after mounting. Metadata from
	// lsblk/udev is discovery, not permission to trust a stale signature.
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			fail("device_access_denied", "the helper lacks permission to open the resolved block device; check its privilege and device access policy")
		}
		fail("device_not_found", "the resolved device could not be opened: "+errnoText(err))
	}
	handle := os.NewFile(uintptr(fd), device)
	defer handle.Close()
	var selected unix.Stat_t
	if err := unix.Fstat(fd, &selected); err != nil || selected.Mode&unix.S_IFMT != unix.S_IFBLK {
		fail("device_not_found", "the resolved path is not a block device")
	}
	if err := verifyFilesystem(ctx, runner, handle, normalizedUUID, normalizedType); err != nil {
		fail("identity_mismatch", err.Error())
	}

	if err := prepareMountPoint(mountPoint); err != nil {
		fail("mount_point_unsafe", err.Error())
	}
	if mounted, _ := isMountPoint(mountPoint); mounted {
		fail("already_mounted", "something is already mounted at the ROTUP mount point")
	}

	if err := unix.Mount(device, mountPoint, normalizedType, mountFlags, ""); err != nil {
		_ = os.Remove(mountPoint)
		fail("mount_failed", "the mount call failed: "+errnoText(err))
	}

	// Correlate the actual mounted filesystem's kernel device number with the
	// open device, re-probe the signature, and re-check UUID uniqueness. The
	// daemon still independently verifies mountinfo and fresh enumeration.
	var mounted unix.Stat_t
	verificationErr := unix.Stat(mountPoint, &mounted)
	if verificationErr == nil && mounted.Dev != selected.Rdev {
		verificationErr = errors.New("the mounted filesystem is not the selected block device")
	}
	if verificationErr == nil {
		verificationErr = verifyFilesystem(ctx, runner, handle, normalizedUUID, normalizedType)
	}
	if verificationErr == nil {
		current, err := enumerator.ResolveFilesystem(ctx, normalizedUUID)
		verificationErr = err
		if err == nil && (current.DevicePath != device || current.FilesystemType != normalizedType) {
			verificationErr = errors.New("the filesystem identity changed while mounting")
		}
	}
	if verificationErr != nil {
		if err := unix.Unmount(mountPoint, 0); err != nil {
			fail("identity_mismatch", "mounted identity could not be verified and cleanup unmount failed")
		}
		_ = os.Remove(mountPoint)
		fail("identity_mismatch", "mounted identity could not be verified: "+verificationErr.Error())
	}

	succeed(media.Result{
		OK:             true,
		Operation:      media.CommandMount,
		MountPoint:     mountPoint,
		ResolvedDevice: device,
		FilesystemUUID: normalizedUUID,
		FilesystemType: normalizedType,
		Mounted:        true,
	})
}

func doSync(mountPoint string) {
	mounted, err := isMountPoint(mountPoint)
	if err != nil {
		fail("mount_point_unreadable", "the ROTUP mount point could not be inspected")
	}
	if !mounted {
		fail("not_mounted", "nothing is mounted at the ROTUP mount point")
	}

	handle, err := os.Open(mountPoint)
	if err != nil {
		fail("sync_failed", "the mounted filesystem could not be opened for flushing")
	}
	defer handle.Close()

	// syncfs flushes exactly this filesystem, unlike sync(2) which flushes
	// every mounted filesystem on the host.
	if err := unix.Syncfs(int(handle.Fd())); err != nil {
		fail("sync_failed", "the filesystem flush failed: "+errnoText(err))
	}
	succeed(media.Result{
		OK: true, Operation: media.CommandSync, MountPoint: mountPoint, Mounted: true,
	})
}

func doUnmount(mountPoint string) {
	mounted, err := isMountPoint(mountPoint)
	if err != nil {
		fail("mount_point_unreadable", "the ROTUP mount point could not be inspected")
	}
	if !mounted {
		// Already gone is the desired end state, so this is reported as
		// success rather than as an error the caller has to special-case.
		_ = os.Remove(mountPoint)
		succeed(media.Result{
			OK: true, Operation: media.CommandUnmount, MountPoint: mountPoint, Mounted: false,
		})
	}

	// Flags are deliberately zero: no MNT_FORCE and no MNT_DETACH. A lazy
	// unmount would return success while the filesystem is still live, which is
	// precisely the false "safe to remove" ROTUP must never report.
	if err := unix.Unmount(mountPoint, 0); err != nil {
		fail("unmount_failed", "the unmount call failed: "+errnoText(err))
	}
	if stillMounted, err := isMountPoint(mountPoint); err != nil || stillMounted {
		fail("unmount_failed", "the mount point is still a mount after unmounting")
	}
	_ = os.Remove(mountPoint)

	succeed(media.Result{
		OK: true, Operation: media.CommandUnmount, MountPoint: mountPoint, Mounted: false,
	})
}

// prepareMountPoint creates the mount root and the per-disk directory, and
// refuses anything that is not a plain, empty, ROTUP-owned directory.
func prepareMountPoint(mountPoint string) error {
	if err := os.MkdirAll(media.MountRoot, media.MountRootMode); err != nil {
		return errors.New("the mount root could not be created")
	}
	if err := os.Chmod(media.MountRoot, media.MountRootMode); err != nil {
		return errors.New("the mount root permissions could not be set")
	}

	info, err := os.Lstat(mountPoint)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(mountPoint, media.MountPointMode); err != nil {
			return errors.New("the mount point could not be created")
		}
		return nil
	case err != nil:
		return errors.New("the mount point could not be inspected")
	}

	// Lstat rather than Stat: a symbolic link here would be an attempt to
	// redirect the mount somewhere else entirely.
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("the mount point is a symbolic link")
	}
	if !info.IsDir() {
		return errors.New("the mount point exists and is not a directory")
	}
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		return errors.New("the mount point could not be listed")
	}
	if len(entries) != 0 {
		return errors.New("the mount point is not empty")
	}
	if err := os.Chmod(mountPoint, media.MountPointMode); err != nil {
		return errors.New("the mount point permissions could not be set")
	}
	return nil
}

// isMountPoint reports whether path is a mount point, by comparing its device
// number with its parent's. This reads kernel state rather than a mount table
// file, so a stale or unreadable /proc cannot make it answer wrongly.
func isMountPoint(path string) (bool, error) {
	var here, parent unix.Stat_t
	if err := unix.Lstat(path, &here); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	if err := unix.Lstat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return here.Dev != parent.Dev, nil
}

func errnoText(err error) string {
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "unknown error"
}

func succeed(result media.Result) {
	emit(result)
	os.Exit(0)
}

func fail(code, message string) {
	emit(media.Result{OK: false, ErrorCode: code, ErrorMessage: message})
	os.Exit(1)
}

func emit(result media.Result) {
	encoded, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the helper result could not be encoded")
		os.Exit(2)
	}
	fmt.Println(string(encoded))
}
