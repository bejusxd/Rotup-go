package restic

import (
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// restic's documented exit codes. They are coarse and version-dependent, so the
// classifier also inspects the already redacted diagnostic text before deciding.
const (
	exitSuccess = 0
	// exitCommandFailed is restic's generic failure.
	exitCommandFailed = 1
	// exitGoRuntimeError is an internal restic failure.
	exitGoRuntimeError = 2
	// exitSourceUnreadable means restic completed a snapshot but could not read
	// some of the requested source data. ROTUP treats it as a failure: a backup
	// that silently omitted files is not the backup that was asked for.
	exitSourceUnreadable = 3
	// exitRepositoryMissing, exitRepositoryLocked and exitWrongPassword exist
	// from restic 0.17 onwards. Older versions report these through exit code 1
	// plus a message, which the text markers below still catch.
	exitRepositoryMissing = 10
	exitRepositoryLocked  = 11
	exitWrongPassword     = 12
	// exitInterrupted is the conventional shell code for SIGINT.
	exitInterrupted = 130
)

// passwordMarkers identify a repository that could not be decrypted.
var passwordMarkers = []string{
	"wrong password",
	"is not a valid repository",
	"invalid password",
}

// missingRepositoryMarkers identify a repository that is not there at all.
var missingRepositoryMarkers = []string{
	"unable to open config file",
	"repository does not exist",
	"no such file or directory",
	"config file not found",
}

// existingRepositoryMarkers identify an initialization refused because a
// repository is already present.
var existingRepositoryMarkers = []string{
	"config file already exists",
	"repository master key and config already initialized",
	"already initialized",
}

// lockMarkers identify a repository another process is holding.
var lockMarkers = []string{
	"repository is already locked",
	"unable to create lock",
}

// capacityMarkers identify a full destination.
var capacityMarkers = []string{
	"no space left on device",
	"disk quota exceeded",
}

// mediaLossMarkers identify the media disappearing underneath restic. They are
// the signature of a surprise removal during an operation.
var mediaLossMarkers = []string{
	"input/output error",
	"structure needs cleaning",
	"transport endpoint is not connected",
	"read-only file system",
	"stale file handle",
}

// classify maps a finished restic invocation to a typed ROTUP error. text must
// already be redacted. It returns nil only when the invocation succeeded.
func classify(operation string, result ports.ProcessResult, text string) *domain.Error {
	switch {
	case result.TimedOut:
		return domain.NewError(domain.ErrClassTimeout, "restic_timeout",
			operation+" exceeded its time limit and was stopped")
	case result.Canceled:
		return domain.NewError(domain.ErrClassCancellation, "restic_canceled",
			operation+" was canceled before it finished")
	case result.Err != nil:
		return domain.WrapError(domain.ErrClassExternalTool, "restic_start_failed",
			"the restic executable could not be run", result.Err)
	case !result.Exited:
		return domain.NewError(domain.ErrClassExternalTool, "restic_no_exit",
			"restic did not report an exit status")
	case result.ExitCode == exitSuccess:
		return nil
	}

	lowered := strings.ToLower(text)
	switch {
	case containsAny(lowered, capacityMarkers):
		return domain.NewError(domain.ErrClassCapacity, "restic_no_space",
			operation+" stopped because the repository filesystem is full")
	case containsAny(lowered, existingRepositoryMarkers):
		return domain.NewError(domain.ErrClassIntegrity, "restic_repository_exists",
			operation+" was refused because a restic repository already exists at that location")
	case containsAny(lowered, passwordMarkers):
		return domain.NewError(domain.ErrClassAuthentication, "restic_wrong_password",
			operation+" could not decrypt the repository: the stored repository password is not the right one")
	case containsAny(lowered, lockMarkers):
		return domain.NewError(domain.ErrClassUnavailable, "restic_repository_locked",
			operation+" could not proceed because the repository is locked by another operation")
	case containsAny(lowered, mediaLossMarkers):
		return domain.NewError(domain.ErrClassIntegrity, "restic_media_error",
			operation+" failed with a filesystem error from the media; the repository must be treated as suspect")
	case containsAny(lowered, missingRepositoryMarkers):
		return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorNotEstablished,
			operation+" could not open a restic repository at the expected location")
	}

	switch result.ExitCode {
	case exitWrongPassword:
		return domain.NewError(domain.ErrClassAuthentication, "restic_wrong_password",
			operation+" could not decrypt the repository: the stored repository password is not the right one")
	case exitRepositoryMissing:
		return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorNotEstablished,
			operation+" could not open a restic repository at the expected location")
	case exitRepositoryLocked:
		return domain.NewError(domain.ErrClassUnavailable, "restic_repository_locked",
			operation+" could not proceed because the repository is locked by another operation")
	case exitSourceUnreadable:
		return domain.NewError(domain.ErrClassIntegrity, "restic_source_unreadable",
			operation+" could not read part of the staging data it was asked to back up, so the snapshot is incomplete")
	case exitInterrupted:
		return domain.NewError(domain.ErrClassCancellation, "restic_interrupted",
			operation+" was interrupted before it finished")
	case exitGoRuntimeError:
		return domain.NewError(domain.ErrClassExternalTool, "restic_internal_error",
			operation+" failed with an internal restic error")
	case exitCommandFailed:
		return domain.NewError(domain.ErrClassExternalTool, "restic_command_failed",
			operation+" failed")
	default:
		return domain.NewError(domain.ErrClassExternalTool, "restic_unknown_exit",
			operation+" failed with an unrecognized restic exit status")
	}
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func errEngineUnconfigured() error {
	return domain.NewError(domain.ErrClassConfiguration, "restic_unconfigured",
		"no restic executable is configured; set ROTUP_RESTIC_PATH")
}
