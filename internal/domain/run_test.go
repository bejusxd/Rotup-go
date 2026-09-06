package domain

import "testing"

// Adding statuses to the run state machine is only safe if their classification
// is decided deliberately. Two mistakes matter, and neither would be obvious:
// an active status wrongly called terminal is a run the startup sweep will never
// look at, which after a crash leaves a backup repository that is never marked
// suspect; and a non-success status wrongly called successful would let a run
// that never finished claim coverage.
func TestRunStatusClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status     RunStatus
		terminal   bool
		successful bool
	}{
		{RunStatusPending, false, false},
		{RunStatusWaitingForResource, false, false},
		{RunStatusQueued, false, false},
		{RunStatusPreflight, false, false},
		{RunStatusRunning, false, false},
		{RunStatusFinalizing, false, false},
		{RunStatusRetryDelay, false, false},
		{RunStatusSucceeded, true, true},
		{RunStatusSucceededWithWarning, true, true},
		{RunStatusPartial, true, false},
		{RunStatusFailed, true, false},
		{RunStatusTimedOut, true, false},
		{RunStatusInterrupted, true, false},
		{RunStatusCanceled, true, false},
		{RunStatusSkipped, true, false},
		{RunStatusSuperseded, true, false},
	}
	for _, c := range cases {
		if got := c.status.Terminal(); got != c.terminal {
			t.Errorf("%s.Terminal() = %v, want %v", c.status, got, c.terminal)
		}
		if got := c.status.Successful(); got != c.successful {
			t.Errorf("%s.Successful() = %v, want %v", c.status, got, c.successful)
		}
	}
}

// Every status the schema accepts must be classified explicitly. A value that
// falls through to a default is the failure mode this guards: it would be
// classified by accident rather than by decision.
func TestEverySchemaRunStatusIsClassified(t *testing.T) {
	t.Parallel()

	// The status set migration 005 permits, written out independently of the
	// constants so a rename cannot make this agree with itself.
	schema := []string{
		"PENDING", "WAITING_FOR_RESOURCE", "QUEUED", "PREFLIGHT", "RUNNING",
		"FINALIZING", "RETRY_DELAY", "SUCCEEDED", "SUCCEEDED_WITH_WARNING",
		"PARTIAL", "FAILED", "TIMED_OUT", "INTERRUPTED", "CANCELED",
		"SKIPPED", "SUPERSEDED",
	}
	known := map[RunStatus]bool{
		RunStatusPending: true, RunStatusWaitingForResource: true,
		RunStatusQueued: true, RunStatusPreflight: true, RunStatusRunning: true,
		RunStatusFinalizing: true, RunStatusRetryDelay: true,
		RunStatusSucceeded: true, RunStatusSucceededWithWarning: true,
		RunStatusPartial: true, RunStatusFailed: true, RunStatusTimedOut: true,
		RunStatusInterrupted: true, RunStatusCanceled: true,
		RunStatusSkipped: true, RunStatusSuperseded: true,
	}
	if len(known) != len(schema) {
		t.Fatalf("%d declared statuses against %d in the schema", len(known), len(schema))
	}
	for _, value := range schema {
		if !known[RunStatus(value)] {
			t.Errorf("the schema accepts %q but the domain declares no constant for it", value)
		}
	}
}

// An unrecognised status must be treated as unfinished. Sweeping a run that was
// actually complete is visible and recoverable; ignoring one that was not is
// the silent case.
func TestUnknownRunStatusIsTreatedAsActive(t *testing.T) {
	t.Parallel()

	unknown := RunStatus("SOMETHING_A_LATER_VERSION_WRITES")
	if unknown.Terminal() {
		t.Error("an unrecognised status was reported as finished")
	}
	if unknown.Successful() {
		t.Error("an unrecognised status was reported as successful")
	}
}

// Which kinds startup recovery owns. RETENTION is classified now, before M8
// produces one, so that milestone cannot begin writing repository-mutating runs
// while the sweep silently ignores them.
func TestRunKindRepositoryOwnership(t *testing.T) {
	t.Parallel()

	for _, kind := range []RunKind{
		RunKindDiskSetup, RunKindBackupLatest, RunKindVerify, RunKindRetention,
	} {
		if !kind.TouchesRepository() {
			t.Errorf("%s should touch the repository", kind)
		}
	}
	for _, kind := range []RunKind{RunKindDemo, RunKindConnectionTest, RunKindCollection} {
		if kind.TouchesRepository() {
			t.Errorf("%s should not touch the repository", kind)
		}
	}
}
