package domain

import (
	"strings"
	"time"
	"unicode"
)

// DiskStatus is the persisted lifecycle state of a registered disk. The values
// come from the architecture's disk state machine; M3 reaches the media
// lifecycle subset, and the run-oriented states are reserved for later
// milestones so the enum does not have to be rebuilt when they arrive.
type DiskStatus string

const (
	// DiskDisconnected means no currently observed filesystem carries the
	// registered UUID.
	DiskDisconnected DiskStatus = "DISCONNECTED"
	// DiskDetected means the filesystem was observed and identified but is not
	// being offered for work, because the disk is disabled.
	DiskDetected DiskStatus = "DETECTED"
	// DiskAvailableUnmounted is the normal idle state of a connected,
	// registered, enabled disk. A disk that is not being worked on belongs
	// here, not mounted.
	DiskAvailableUnmounted DiskStatus = "AVAILABLE_UNMOUNTED"
	// DiskValidating means identity and safety checks are running before a
	// mount is attempted.
	DiskValidating DiskStatus = "VALIDATING"
	// DiskMounting means the mount is in progress.
	DiskMounting DiskStatus = "MOUNTING"
	// DiskReady means the disk is mounted at its ROTUP mount point and its
	// identity was proven after mounting.
	DiskReady DiskStatus = "READY"
	// DiskBackupRunning means a BACKUP_LATEST run is writing to this disk's
	// repository.
	DiskBackupRunning DiskStatus = "BACKUP_RUNNING"
	// DiskVerifying means a non-destructive repository verification is running.
	DiskVerifying DiskStatus = "VERIFYING"
	// DiskMaintenanceRunning means a repository maintenance operation is
	// running. Nothing in v1 reaches it yet; retention arrives with M8.
	DiskMaintenanceRunning DiskStatus = "MAINTENANCE_RUNNING"
	// DiskSyncing means a filesystem flush is in progress.
	DiskSyncing DiskStatus = "SYNCING"
	// DiskUnmounting means the unmount is in progress.
	DiskUnmounting DiskStatus = "UNMOUNTING"
	// DiskSafeToRemove means the ROTUP mount is verifiably gone after a
	// successful sync and unmount. The next reconciliation moves a still
	// connected disk on to AVAILABLE_UNMOUNTED.
	DiskSafeToRemove DiskStatus = "SAFE_TO_REMOVE"
	// DiskError means the disk needs operator attention. LastErrorCode says why.
	DiskError DiskStatus = "ERROR"
)

// Busy reports whether a media operation is currently in progress. Statuses in
// this set are owned by the running operation and must not be recomputed by
// reconciliation underneath it.
func (s DiskStatus) Busy() bool {
	switch s {
	case DiskValidating, DiskMounting, DiskSyncing, DiskUnmounting,
		DiskBackupRunning, DiskVerifying, DiskMaintenanceRunning:
		return true
	default:
		return false
	}
}

// Mounted reports whether ROTUP believes its own mount is currently in place.
// Every status in which a repository operation is running implies the mount is
// still there, because those operations are the reason it was mounted.
func (s DiskStatus) Mounted() bool {
	switch s {
	case DiskReady, DiskBackupRunning, DiskVerifying, DiskMaintenanceRunning:
		return true
	default:
		return false
	}
}

// Error codes recorded on a disk. They are stable and administrator-facing.
const (
	DiskErrorAmbiguousIdentity      = "ambiguous_identity"
	DiskErrorUnsupportedFilesystem  = "unsupported_filesystem"
	DiskErrorExternalMount          = "external_mount"
	DiskErrorMountFailed            = "mount_failed"
	DiskErrorIdentityMismatch       = "identity_mismatch"
	DiskErrorSyncFailed             = "sync_failed"
	DiskErrorUnmountFailed          = "unmount_failed"
	DiskErrorRemovedDuringOperation = "removed_during_operation"
)

// Disk is a registered backup disk. Its durable identity is the filesystem
// UUID; nothing about a device node is ever stored here, because a device node
// is an observation that is only valid until the next enumeration.
type Disk struct {
	ID             string
	Name           string
	FilesystemUUID string
	// FilesystemType is the type recorded at registration. It is the expected
	// type, and a later observation that disagrees is an identity problem.
	FilesystemType     string
	Enabled            bool
	FirstRegisteredAt  time.Time
	LastConnectedAt    *time.Time
	LastDisconnectedAt *time.Time
	CurrentStatus      DiskStatus
	LastErrorCode      string
	UpdatedAt          time.Time
}

// DiskState is the mutable operational state of a registered disk. It is
// separated from Disk so that a reconciliation writes only what it actually
// observed, and never silently rewrites administrator-owned configuration.
type DiskState struct {
	ID        string
	Status    DiskStatus
	ErrorCode string
	// LastConnectedAt and LastDisconnectedAt are set only on the transition
	// that justifies them; a nil pointer leaves the stored value alone.
	LastConnectedAt    *time.Time
	LastDisconnectedAt *time.Time
}

// ObservedMedia is one currently observed filesystem-bearing block device. It
// is entirely ephemeral: it describes what the kernel reports right now, and
// every field including the device path is invalid the moment the hardware
// changes. It is never persisted as identity.
type ObservedMedia struct {
	// DevicePath is the current device node. It is a diagnostic, never an
	// identity, and must be re-resolved on every enumeration.
	DevicePath string
	// FilesystemUUID is the persistent identity ROTUP registers against.
	FilesystemUUID string
	FilesystemType string
	Label          string
	SizeBytes      int64
	// Removable and Transport carry the kernel's own classification where it is
	// available. They are advisory: ROTUP never decides to write to a disk
	// because it looks removable.
	Removable bool
	Transport string
	ReadOnly  bool
	// ParentDevice is the whole-disk device this partition belongs to, used
	// only for display.
	ParentDevice string
	ObservedAt   time.Time
}

// Identified reports whether the observation carries a usable persistent
// identity. Media without a filesystem UUID can be shown but never registered.
func (m ObservedMedia) Identified() bool { return m.FilesystemUUID != "" }

// MountObservation is one entry of the authoritative kernel mount table.
type MountObservation struct {
	// Source is the device node the kernel says is mounted.
	Source string
	// MountPoint is where it is mounted.
	MountPoint     string
	FilesystemType string
	// ReadOnly reports whether the mount itself is read-only.
	ReadOnly bool
}

// SupportedFilesystems is the deliberately narrow v1 set ROTUP will mount.
//
// Only ext4 is supported. It is the native Linux filesystem with a real
// persistent UUID, POSIX ownership and permissions, case-sensitive filenames,
// a journal that survives surprise removal predictably, and well-defined
// syncfs and unmount semantics. Those are exactly the properties the backup
// repository will depend on.
//
// exFAT and NTFS are deliberately excluded from v1. exFAT identifies itself
// with a short volume serial rather than a UUID and is case-insensitive with no
// POSIX ownership; NTFS additionally introduces a choice between the in-kernel
// ntfs3 driver and the ntfs-3g FUSE driver, each with different unattended
// reliability characteristics. Both are observed and shown, and neither is ever
// mounted. Adding a filesystem is an allowlist change plus a run of the Linux
// integration spike against it.
var SupportedFilesystems = []string{"ext4"}

// FilesystemSupported reports whether ROTUP will mount this filesystem type.
func FilesystemSupported(filesystemType string) bool {
	normalized := NormalizeFilesystemType(filesystemType)
	for _, supported := range SupportedFilesystems {
		if normalized == supported {
			return true
		}
	}
	return false
}

// NormalizeFilesystemType lower-cases and trims a reported filesystem type.
func NormalizeFilesystemType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// NormalizeFilesystemUUID trims and lower-cases a filesystem UUID so that
// comparisons and uniqueness are case-insensitive, matching the architecture's
// COLLATE NOCASE constraint.
func NormalizeFilesystemUUID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

const maxFilesystemUUIDLength = 64

// ValidateFilesystemUUID checks that a UUID is present and is built only from
// characters that can safely become part of a /dev/disk/by-uuid lookup. It
// deliberately accepts more than the RFC 4122 shape, because real filesystems
// use several forms: ext4 uses a full UUID, and other types use shorter
// serials. What it refuses is anything that could be read as a path or a flag.
func ValidateFilesystemUUID(value string) (string, error) {
	normalized := NormalizeFilesystemUUID(value)
	if normalized == "" {
		return "", NewError(ErrClassConfiguration, "uuid_missing",
			"this media has no persistent filesystem UUID, so it cannot be registered")
	}
	if len(normalized) > maxFilesystemUUIDLength {
		return "", NewError(ErrClassConfiguration, "uuid_invalid",
			"the filesystem UUID is longer than ROTUP accepts")
	}
	for _, r := range normalized {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex && r != '-' {
			return "", NewError(ErrClassConfiguration, "uuid_invalid",
				"the filesystem UUID contains characters ROTUP does not accept")
		}
	}
	return normalized, nil
}

// ValidateDiskName checks the administrator-supplied friendly name.
func ValidateDiskName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", NewError(ErrClassConfiguration, "disk_name_empty", "the disk name must not be empty")
	}
	if len(trimmed) > maxNameLength {
		return "", NewError(ErrClassConfiguration, "disk_name_invalid", "the disk name is too long")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", NewError(ErrClassConfiguration, "disk_name_invalid",
				"the disk name must not contain control characters")
		}
	}
	return trimmed, nil
}
