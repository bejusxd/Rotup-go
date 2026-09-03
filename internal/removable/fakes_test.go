package removable

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/media"
	"github.com/bejusxd/rotup/internal/ports"
)

// fakeSystem models the parts of a host ROTUP observes: a set of block devices
// and a kernel mount table. Tests manipulate it the way hardware and an
// administrator would — plugging media in, pulling it out, mounting it
// elsewhere — and the service is expected to converge on the truth it presents.
type fakeSystem struct {
	mu sync.Mutex

	devices []domain.ObservedMedia
	mounts  []domain.MountObservation

	enumerateErr error
	mountErr     error
	syncErr      error
	unmountErr   error

	// mountReportsUUID overrides what the mount call claims to have mounted,
	// so a platform that lies about identity can be tested.
	mountReportsUUID string
	// mountAttachesUUID overrides which device actually appears at the mount
	// point, modelling a mount that succeeded but landed on other hardware.
	mountAttachesUUID string
	// removeOnSync and removeOnUnmount pull the device mid-operation.
	removeOnSync    bool
	removeOnUnmount bool
	// unmountLeavesMounted models an unmount that reports success without
	// actually detaching anything.
	unmountLeavesMounted bool

	mountCalls   int
	syncCalls    int
	unmountCalls int
	available    bool
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{available: true}
}

func (f *fakeSystem) attach(item domain.ObservedMedia) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = append(f.devices, item)
}

func (f *fakeSystem) detachUUID(uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []domain.ObservedMedia
	var removedPaths []string
	for _, device := range f.devices {
		if device.FilesystemUUID == uuid {
			removedPaths = append(removedPaths, device.DevicePath)
			continue
		}
		kept = append(kept, device)
	}
	f.devices = kept
	// A device that is physically gone takes its mounts with it.
	var keptMounts []domain.MountObservation
	for _, mount := range f.mounts {
		removed := false
		for _, path := range removedPaths {
			if mount.Source == path {
				removed = true
			}
		}
		if !removed {
			keptMounts = append(keptMounts, mount)
		}
	}
	f.mounts = keptMounts
}

// renameDevice models the device node moving between enumerations, which is
// exactly what happens when hardware is re-plugged in a different order.
func (f *fakeSystem) renameDevice(uuid, newPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.devices {
		if f.devices[i].FilesystemUUID == uuid {
			old := f.devices[i].DevicePath
			f.devices[i].DevicePath = newPath
			for j := range f.mounts {
				if f.mounts[j].Source == old {
					f.mounts[j].Source = newPath
				}
			}
		}
	}
}

func (f *fakeSystem) mountElsewhere(uuid, mountPoint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, device := range f.devices {
		if device.FilesystemUUID == uuid {
			f.mounts = append(f.mounts, domain.MountObservation{
				Source: device.DevicePath, MountPoint: mountPoint,
				FilesystemType: device.FilesystemType,
			})
		}
	}
}

func (f *fakeSystem) mountPoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var points []string
	for _, mount := range f.mounts {
		points = append(points, mount.MountPoint)
	}
	return points
}

// Enumerate implements ports.MediaEnumerator.
func (f *fakeSystem) Enumerate(context.Context) ([]domain.ObservedMedia, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enumerateErr != nil {
		return nil, f.enumerateErr
	}
	return append([]domain.ObservedMedia(nil), f.devices...), nil
}

// Available implements ports.MountManager.
func (f *fakeSystem) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

// MountPointFor derives the same path the real platform would.
func (f *fakeSystem) MountPointFor(diskID string) (string, error) {
	point, err := media.MountPointFor(diskID)
	if err != nil {
		return "", domain.WrapError(domain.ErrClassInvariant, "mount_point_invalid",
			"the disk identifier cannot be turned into a mount point", err)
	}
	return point, nil
}

// Observe returns the fake kernel mount table.
func (f *fakeSystem) Observe(context.Context) ([]domain.MountObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.MountObservation(nil), f.mounts...), nil
}

// Mount attaches the device at the ROTUP mount point.
func (f *fakeSystem) Mount(_ context.Context, request ports.MountRequest) (ports.MountResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mountCalls++
	if f.mountErr != nil {
		return ports.MountResult{}, f.mountErr
	}
	point, err := media.MountPointFor(request.DiskID)
	if err != nil {
		return ports.MountResult{}, err
	}

	attachUUID := request.FilesystemUUID
	if f.mountAttachesUUID != "" {
		attachUUID = f.mountAttachesUUID
	}
	var attached *domain.ObservedMedia
	for i := range f.devices {
		if f.devices[i].FilesystemUUID == attachUUID {
			attached = &f.devices[i]
			break
		}
	}
	if attached == nil {
		return ports.MountResult{}, domain.NewError(domain.ErrClassUnavailable,
			"mount_helper_device_not_found", "no connected filesystem carries that UUID")
	}
	f.mounts = append(f.mounts, domain.MountObservation{
		Source: attached.DevicePath, MountPoint: point, FilesystemType: attached.FilesystemType,
	})

	reported := request.FilesystemUUID
	if f.mountReportsUUID != "" {
		reported = f.mountReportsUUID
	}
	return ports.MountResult{
		MountPoint: point, ResolvedDevice: attached.DevicePath,
		FilesystemUUID: reported, FilesystemType: attached.FilesystemType, Mounted: true,
	}, nil
}

// Sync flushes, or fails, or watches the device vanish.
func (f *fakeSystem) Sync(_ context.Context, diskID string) error {
	f.mu.Lock()
	f.syncCalls++
	shouldRemove := f.removeOnSync
	err := f.syncErr
	f.mu.Unlock()

	if shouldRemove {
		f.removeMountedDevice(diskID)
	}
	return err
}

// Unmount detaches the ROTUP mount.
func (f *fakeSystem) Unmount(_ context.Context, diskID string) error {
	f.mu.Lock()
	f.unmountCalls++
	shouldRemove := f.removeOnUnmount
	err := f.unmountErr
	leaveMounted := f.unmountLeavesMounted
	f.mu.Unlock()

	if shouldRemove {
		f.removeMountedDevice(diskID)
	}
	if err != nil {
		return err
	}
	if leaveMounted {
		return nil
	}

	point, pointErr := media.MountPointFor(diskID)
	if pointErr != nil {
		return pointErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []domain.MountObservation
	for _, mount := range f.mounts {
		if mount.MountPoint != point {
			kept = append(kept, mount)
		}
	}
	f.mounts = kept
	return nil
}

// removeMountedDevice models the disk being physically pulled while an
// operation is running.
func (f *fakeSystem) removeMountedDevice(diskID string) {
	point, err := media.MountPointFor(diskID)
	if err != nil {
		return
	}
	f.mu.Lock()
	source := ""
	for _, mount := range f.mounts {
		if mount.MountPoint == point {
			source = mount.Source
		}
	}
	f.mu.Unlock()
	if source == "" {
		return
	}
	f.mu.Lock()
	uuid := ""
	for _, device := range f.devices {
		if device.DevicePath == source {
			uuid = device.FilesystemUUID
		}
	}
	f.mu.Unlock()
	if uuid != "" {
		f.detachUUID(uuid)
	}
}

// media builds an observation for a test.
func mediaItem(devicePath, uuid, fsType string) domain.ObservedMedia {
	return domain.ObservedMedia{
		DevicePath:     devicePath,
		FilesystemUUID: uuid,
		FilesystemType: fsType,
		SizeBytes:      512 * 1024 * 1024 * 1024,
		Removable:      true,
		Transport:      "usb",
		ObservedAt:     time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC),
	}
}

const (
	uuidA = "11111111-2222-3333-4444-555555555555"
	uuidB = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func hasMountAt(points []string, wanted string) bool {
	for _, point := range points {
		if point == wanted {
			return true
		}
	}
	return false
}

func containsCode(err error, code string) bool {
	var typed *domain.Error
	if !domain.AsError(err, &typed) {
		return false
	}
	return typed.Code == code || strings.Contains(typed.Code, code)
}
