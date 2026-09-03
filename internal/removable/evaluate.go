// Package removable owns the removable-media use cases: reconciling what the
// system currently presents against what ROTUP has registered, explicit
// registration, and the explicit mount, sync and unmount lifecycle.
//
// Its central rule is that state describes reality rather than intent. Every
// status is derived from a fresh authoritative observation, and nothing ROTUP
// remembered about a device node is ever trusted: a disk is found by its
// filesystem UUID, every time.
package removable

import (
	"github.com/bejusxd/rotup/internal/domain"
)

// evaluation is the status a registered disk should currently hold, derived
// entirely from live observation.
type evaluation struct {
	Status    domain.DiskStatus
	ErrorCode string
	// Observed is the single matching observation, or nil when the disk is
	// absent or its identity is ambiguous.
	Observed *domain.ObservedMedia
	// Ambiguous records that more than one connected filesystem claims the
	// registered UUID.
	Ambiguous bool
	// MountPoint is ROTUP's own mount point when the disk is mounted there.
	MountPoint string
	// ExternalMounts lists mount points outside ROTUP's mount root where this
	// filesystem is currently mounted.
	ExternalMounts []string
}

// Connected reports whether the disk's filesystem is currently present at all,
// including the ambiguous case where several candidates claim its UUID.
func (e evaluation) Connected() bool {
	return e.Status != domain.DiskDisconnected
}

// matchingMedia returns every observation carrying a given UUID. More than one
// result is an ambiguity that must block action rather than be resolved by
// picking a winner.
func matchingMedia(observations []domain.ObservedMedia, filesystemUUID string) []domain.ObservedMedia {
	wanted := domain.NormalizeFilesystemUUID(filesystemUUID)
	if wanted == "" {
		return nil
	}
	var matches []domain.ObservedMedia
	for _, observation := range observations {
		if domain.NormalizeFilesystemUUID(observation.FilesystemUUID) == wanted {
			matches = append(matches, observation)
		}
	}
	return matches
}

// mountsForDevice returns the mount table entries whose source is a device.
func mountsForDevice(mounts []domain.MountObservation, devicePath string) []domain.MountObservation {
	if devicePath == "" {
		return nil
	}
	var matches []domain.MountObservation
	for _, mount := range mounts {
		if mount.Source == devicePath {
			matches = append(matches, mount)
		}
	}
	return matches
}

// evaluate derives a registered disk's current status.
//
// The ordering matters and is deliberate. Absence and ambiguity are settled
// first, because neither permits any further conclusion. A mount ROTUP does not
// own is next, because adopting or ignoring it would both be wrong. Only then
// does the disk's own configuration decide whether it is available.
func evaluate(disk domain.Disk, observations []domain.ObservedMedia,
	mounts []domain.MountObservation, expectedMountPoint string) evaluation {

	matches := matchingMedia(observations, disk.FilesystemUUID)
	switch len(matches) {
	case 0:
		return evaluation{Status: domain.DiskDisconnected}
	case 1:
	default:
		// Several connected filesystems claim this UUID. ROTUP must not pick
		// one: choosing by device order or by which was seen first would mean
		// writing a backup to whichever disk happened to enumerate first.
		return evaluation{
			Status:    domain.DiskError,
			ErrorCode: domain.DiskErrorAmbiguousIdentity,
			Ambiguous: true,
		}
	}

	observed := matches[0]
	result := evaluation{Observed: &observed}

	var external []string
	rotupMounts := 0
	for _, mount := range mountsForDevice(mounts, observed.DevicePath) {
		if expectedMountPoint != "" && mount.MountPoint == expectedMountPoint {
			rotupMounts++
			continue
		}
		external = append(external, mount.MountPoint)
	}
	result.ExternalMounts = external

	// A filesystem mounted somewhere ROTUP does not own is not ROTUP's to use
	// or to unmount. It is reported and refused, never taken over.
	if len(external) > 0 {
		result.Status = domain.DiskError
		result.ErrorCode = domain.DiskErrorExternalMount
		return result
	}
	if rotupMounts > 0 {
		result.Status = domain.DiskReady
		result.MountPoint = expectedMountPoint
		return result
	}

	if !disk.Enabled {
		// The disk is identified but deliberately not offered for work. This is
		// an administrator's choice, not a fault, so it is not an error state.
		result.Status = domain.DiskDetected
		return result
	}
	if !domain.FilesystemSupported(observed.FilesystemType) {
		result.Status = domain.DiskError
		result.ErrorCode = domain.DiskErrorUnsupportedFilesystem
		return result
	}
	if disk.FilesystemType != "" &&
		domain.NormalizeFilesystemType(disk.FilesystemType) != observed.FilesystemType {
		// The UUID matches but the filesystem is not the one that was
		// registered, which means this is not the disk ROTUP thinks it is.
		result.Status = domain.DiskError
		result.ErrorCode = domain.DiskErrorIdentityMismatch
		return result
	}

	result.Status = domain.DiskAvailableUnmounted
	return result
}

// ambiguousUUIDs returns every UUID that more than one currently observed
// filesystem claims. Registration and mounting both refuse these outright.
func ambiguousUUIDs(observations []domain.ObservedMedia) map[string]int {
	counts := make(map[string]int)
	for _, observation := range observations {
		uuid := domain.NormalizeFilesystemUUID(observation.FilesystemUUID)
		if uuid == "" {
			continue
		}
		counts[uuid]++
	}
	for uuid, count := range counts {
		if count < 2 {
			delete(counts, uuid)
		}
	}
	return counts
}
