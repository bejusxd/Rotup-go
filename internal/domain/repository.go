package domain

import (
	"strings"
	"time"
)

// RepositoryHealth is the durable claim ROTUP makes about a restic repository
// on a registered disk. It is deliberately conservative: a repository only
// reaches HEALTHY after ROTUP has proven, in the same operation, that it could
// open the repository and read its identity back.
type RepositoryHealth string

const (
	// RepositoryUninitialized means a repository record exists but no restic
	// repository has been created or adopted on the media yet.
	RepositoryUninitialized RepositoryHealth = "UNINITIALIZED"
	// RepositoryInitializing means an initialization or adoption is in
	// progress. A record left in this state by a crash cannot be trusted,
	// because the daemon may have died between creating the repository on the
	// media and proving it.
	RepositoryInitializing RepositoryHealth = "INITIALIZING"
	// RepositoryHealthy means the repository was last opened successfully and
	// its restic identity matched what ROTUP recorded.
	RepositoryHealthy RepositoryHealth = "HEALTHY"
	// RepositorySuspect means an operation ended without proving the
	// repository's state: an interrupted backup, a failed flush, or an unmount
	// that could not be confirmed. No further write is permitted until an
	// explicit recovery proves the repository again.
	RepositorySuspect RepositoryHealth = "SUSPECT"
	// RepositoryRecoveryRequired means recovery ran and could not prove the
	// repository, or its identity did not match what ROTUP recorded.
	RepositoryRecoveryRequired RepositoryHealth = "RECOVERY_REQUIRED"
	// RepositoryUnavailable means the repository could not be reached at all,
	// normally because its disk is not connected.
	RepositoryUnavailable RepositoryHealth = "UNAVAILABLE"
)

// WritableForBackup reports whether a backup may run against a repository in
// this state. Only a repository ROTUP has proven may be written to; every other
// state requires an explicit administrator action first.
func (h RepositoryHealth) WritableForBackup() bool { return h == RepositoryHealthy }

// RepositoryOrigin records how ROTUP came to own a repository.
type RepositoryOrigin string

const (
	// RepositoryInitialized means ROTUP created the restic repository itself.
	RepositoryInitialized RepositoryOrigin = "INITIALIZED"
	// RepositoryAdopted means an existing restic repository at ROTUP's own
	// repository path was opened, proven, and taken over.
	RepositoryAdopted RepositoryOrigin = "ADOPTED"
)

// RepositoryRelativePath is the ROTUP-owned location of the restic repository
// below a mounted registered disk.
//
// It is a constant rather than a setting. An administrator-supplied repository
// path would be a caller-chosen path crossing into an operation that writes to
// removable media, and it would turn "is this repository ours?" into a question
// about configuration rather than about a fixed, verifiable location. Adoption
// is therefore only ever offered for this one path.
const RepositoryRelativePath = "rotup/restic"

// Repository is a restic repository on one registered disk.
//
// Its durable identity is the pair (registered disk filesystem UUID, restic
// repository ID). Neither a mount point nor a device node appears here: both
// are observations that are only true until the next enumeration.
type Repository struct {
	ID     string
	DiskID string
	// FilesystemUUID is the registered disk's UUID as it stood when the
	// repository was established. It is re-checked against the disk record on
	// every operation so a repository record can never follow a different disk.
	FilesystemUUID string
	// RelativePath is the repository location below the mount point. It is
	// always RepositoryRelativePath; it is stored so an older record stays
	// readable if that constant ever changes in a later milestone.
	RelativePath string
	// CredentialRefID addresses the repository password in the secret store.
	// The password itself never appears in this struct or in SQLite.
	CredentialRefID string
	// ResticRepositoryID is the stable identity restic reports for the
	// repository. An empty value means ROTUP has not proven the repository yet.
	ResticRepositoryID string
	Origin             RepositoryOrigin
	Health             RepositoryHealth
	// SuspectReason is a safe, administrator-facing explanation of why the
	// repository is not currently usable.
	SuspectReason string
	// EscrowAcknowledgedAt records when the administrator confirmed the
	// repository password is recorded outside this host. Without it, losing the
	// host means every snapshot on the media is unrecoverable.
	EscrowAcknowledgedAt   *time.Time
	EstablishedAt          *time.Time
	LastOpenedAt           *time.Time
	LastSnapshotID         string
	LastSuccessfulBackupAt *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Established reports whether a restic repository has actually been proven on
// the media.
func (r Repository) Established() bool { return r.ResticRepositoryID != "" }

const resticRepositoryIDLength = 64

// ValidateResticRepositoryID checks the identity restic reported. restic
// repository IDs are hex digests, and this refuses anything else so that a
// value which was somehow influenced from outside can never be echoed into an
// argument array or a path.
func ValidateResticRepositoryID(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", NewError(ErrClassIntegrity, "repository_id_missing",
			"restic did not report a repository identity, so the repository could not be proven")
	}
	if len(normalized) != resticRepositoryIDLength {
		return "", NewError(ErrClassIntegrity, "repository_id_invalid",
			"restic reported a repository identity ROTUP does not recognize")
	}
	if !hexOnly(normalized) {
		return "", NewError(ErrClassIntegrity, "repository_id_invalid",
			"restic reported a repository identity ROTUP does not recognize")
	}
	return normalized, nil
}

// ValidateSnapshotID checks a restic snapshot identifier. Snapshot IDs are hex
// digests, and a short ID is the first eight characters of one.
func ValidateSnapshotID(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if len(normalized) < 8 || len(normalized) > 64 {
		return "", NewError(ErrClassIntegrity, "snapshot_id_invalid",
			"restic reported a snapshot identity ROTUP does not recognize")
	}
	if !hexOnly(normalized) {
		return "", NewError(ErrClassIntegrity, "snapshot_id_invalid",
			"restic reported a snapshot identity ROTUP does not recognize")
	}
	return normalized, nil
}

func hexOnly(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// MinimumRepositoryPasswordLength is the shortest repository password ROTUP
// accepts.
const MinimumRepositoryPasswordLength = 12

// ValidateRepositoryPassword checks an administrator-supplied repository
// password. A restic repository password cannot be recovered or reset: losing
// it destroys access to every snapshot on that media, so a trivially short one
// is refused rather than accepted with a warning.
func ValidateRepositoryPassword(value string) error {
	if strings.TrimSpace(value) == "" {
		return NewError(ErrClassConfiguration, "repository_password_empty",
			"a repository password is required; restic repositories are always encrypted")
	}
	if len(value) < MinimumRepositoryPasswordLength {
		return NewError(ErrClassConfiguration, "repository_password_weak",
			"the repository password must be at least 12 characters; it cannot be reset or recovered later")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return NewError(ErrClassConfiguration, "repository_password_invalid",
			"the repository password must not contain line breaks or null bytes")
	}
	return nil
}

// Repository error codes. They are stable and administrator-facing.
const (
	RepositoryErrorIdentityMismatch      = "repository_identity_mismatch"
	RepositoryErrorNotEstablished        = "repository_not_established"
	RepositoryErrorUnreadable            = "repository_unreadable"
	RepositoryErrorSnapshotAbsent        = "snapshot_absent"
	RepositoryErrorDurabilityUnproven    = "durability_unproven"
	RepositoryErrorInterrupted           = "repository_interrupted"
	RepositoryErrorPathOccupied          = "repository_path_occupied"
	RepositoryErrorEscrowNotAcknowledged = "escrow_not_acknowledged"
)
