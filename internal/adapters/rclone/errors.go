package rclone

import (
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// rclone's documented exit codes. They are coarse, so the classifier also
// inspects the redacted diagnostic text before deciding.
const (
	exitSuccess              = 0
	exitSyntaxError          = 1
	exitUncategorized        = 2
	exitDirectoryNotFound    = 3
	exitFileNotFound         = 4
	exitTemporaryError       = 5
	exitLessSeriousError     = 6
	exitFatalError           = 7
	exitTransferLimitReached = 8
	exitNoFilesTransferred   = 9
	exitDurationExceeded     = 10
)

// authenticationMarkers are substrings that identify a credential problem
// rather than a general availability problem. They are matched against already
// redacted engine output.
var authenticationMarkers = []string{
	"nt_status_logon_failure",
	"nt_status_access_denied",
	"nt_status_account_locked_out",
	"nt_status_password_expired",
	"nt_status_wrong_password",
	"logon failure",
	"authentication failed",
	"permission denied",
	"access denied",
	"unauthorized",
}

// unavailableMarkers identify a reachability problem.
var unavailableMarkers = []string{
	"connection refused",
	"connection reset",
	"no such host",
	"i/o timeout",
	"network is unreachable",
	"host is down",
	"no route to host",
	"dial tcp",
	"context deadline exceeded",
	"nt_status_bad_network_name",
	"nt_status_host_unreachable",
}

// notFoundMarkers identify a share or subdirectory that does not exist.
var notFoundMarkers = []string{
	"directory not found",
	"not found",
	"nt_status_object_name_not_found",
	"nt_status_object_path_not_found",
	"nt_status_no_such_file",
}

// capacityMarkers identify a full destination.
var capacityMarkers = []string{
	"no space left on device",
	"disk quota exceeded",
	"not enough space",
}

// classify maps a finished rclone invocation to a typed ROTUP error. text must
// already be redacted. It returns nil when the invocation succeeded.
func classify(operation string, result ports.ProcessResult, text string) *domain.Error {
	switch {
	case result.TimedOut:
		return domain.NewError(domain.ErrClassTimeout, "rclone_timeout",
			operation+" exceeded its time limit and was stopped").Retry()
	case result.Canceled:
		return domain.NewError(domain.ErrClassCancellation, "rclone_canceled",
			operation+" was canceled before it finished")
	case result.Err != nil:
		return domain.WrapError(domain.ErrClassExternalTool, "rclone_start_failed",
			"the rclone executable could not be run", result.Err)
	case !result.Exited:
		return domain.NewError(domain.ErrClassExternalTool, "rclone_no_exit",
			"rclone did not report an exit status")
	case result.ExitCode == exitSuccess:
		return nil
	}

	lowered := strings.ToLower(text)
	if containsAny(lowered, capacityMarkers) {
		return domain.NewError(domain.ErrClassCapacity, "rclone_no_space",
			operation+" stopped because the destination filesystem is full")
	}
	if containsAny(lowered, authenticationMarkers) {
		return domain.NewError(domain.ErrClassAuthentication, "rclone_authentication_failed",
			operation+" was rejected by the server: check the username and stored password")
	}
	if containsAny(lowered, unavailableMarkers) {
		return domain.NewError(domain.ErrClassUnavailable, "rclone_unavailable",
			operation+" could not reach the SMB server").Retry()
	}
	if containsAny(lowered, notFoundMarkers) {
		return domain.NewError(domain.ErrClassConfiguration, "rclone_path_not_found",
			operation+" could not find the configured share or subdirectory")
	}

	switch result.ExitCode {
	case exitSyntaxError:
		return domain.NewError(domain.ErrClassInvariant, "rclone_usage_error",
			operation+" was rejected by rclone as a usage error; this is a ROTUP defect")
	case exitDirectoryNotFound, exitFileNotFound:
		return domain.NewError(domain.ErrClassConfiguration, "rclone_path_not_found",
			operation+" could not find the configured share or subdirectory")
	case exitTemporaryError, exitLessSeriousError:
		return domain.NewError(domain.ErrClassUnavailable, "rclone_temporary_error",
			operation+" failed with a temporary error after rclone exhausted its retries").Retry()
	case exitTransferLimitReached, exitNoFilesTransferred:
		return domain.NewError(domain.ErrClassExternalTool, "rclone_transfer_limit",
			operation+" stopped because an rclone transfer limit was reached")
	case exitDurationExceeded:
		return domain.NewError(domain.ErrClassTimeout, "rclone_duration_exceeded",
			operation+" stopped because rclone's own maximum duration was reached").Retry()
	case exitFatalError:
		return domain.NewError(domain.ErrClassExternalTool, "rclone_fatal_error",
			operation+" failed with a fatal rclone error")
	case exitUncategorized:
		return domain.NewError(domain.ErrClassExternalTool, "rclone_error",
			operation+" failed with an rclone error")
	default:
		return domain.NewError(domain.ErrClassExternalTool, "rclone_unknown_exit",
			operation+" failed with an unrecognized rclone exit status")
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
