package domain

import "time"

// DurabilityStage records how far one backup run reached along the path from
// "restic was started" to "the bytes are proven to be on media that has been
// flushed and unmounted".
//
// It exists because a backup has several distinct points of no return and each
// one means something different after a crash. A run that died before restic
// started changed nothing on the media; a run that died after restic reported a
// snapshot may have left a real snapshot behind that recovery can adopt. A
// single run status cannot express that difference honestly, so the stage is
// persisted alongside it.
type DurabilityStage string

const (
	// StageRequested means the run exists and nothing has happened yet.
	StageRequested DurabilityStage = "REQUESTED"
	// StageReadSetCaptured means the exact set of staging state versions and
	// revision batches to be backed up is durably recorded.
	StageReadSetCaptured DurabilityStage = "READ_SET_CAPTURED"
	// StageMounted means the target disk is mounted and its identity proven.
	StageMounted DurabilityStage = "MOUNTED"
	// StageResticRunning means restic was started against the repository. From
	// here on the repository may have been written to, so any failure leaves it
	// suspect until recovery proves otherwise.
	StageResticRunning DurabilityStage = "RESTIC_RUNNING"
	// StageSnapshotCreated means a clean restic result and its snapshot ID
	// were recorded. The snapshot has not yet been independently confirmed.
	StageSnapshotCreated DurabilityStage = "SNAPSHOT_CREATED"
	// StageSnapshotVerified means the snapshot was found in the repository by a
	// separate read, carrying this run's tag.
	StageSnapshotVerified DurabilityStage = "SNAPSHOT_VERIFIED"
	// StageSynced means the filesystem flush completed.
	StageSynced DurabilityStage = "SYNCED"
	// StageUnmountVerified means the ROTUP mount is verifiably gone.
	StageUnmountVerified DurabilityStage = "UNMOUNT_VERIFIED"
	// StageDurableSuccess is the only stage at which a backup may be reported
	// as a normal success.
	StageDurableSuccess DurabilityStage = "DURABLE_SUCCESS"
	// StageFailed is a terminal stage for a run that will not progress.
	StageFailed DurabilityStage = "FAILED"
)

// RepositoryMayHaveChanged reports whether restic was started, and therefore
// whether the repository must be treated as suspect when the run did not reach
// a proven durable end.
func (s DurabilityStage) RepositoryMayHaveChanged() bool {
	switch s {
	case StageResticRunning, StageSnapshotCreated, StageSnapshotVerified,
		StageSynced, StageUnmountVerified:
		return true
	default:
		return false
	}
}

// BackupDurability records how a backup run reached its terminal state.
type BackupDurability string

const (
	// DurabilityNormal means the run passed every stage in order, in one
	// uninterrupted operation.
	DurabilityNormal BackupDurability = "NORMAL"
	// DurabilityAdopted means the run was interrupted before its durability
	// boundary and a later explicit recovery proved the snapshot exists. It is
	// a real success, but it is deliberately not presented as the normal path.
	DurabilityAdopted BackupDurability = "ADOPTED_AFTER_INTERRUPTION"
	// DurabilityUnproven means the run has not been shown to be durable.
	DurabilityUnproven BackupDurability = "UNPROVEN"
)

// BackupRun is the durable detail of one BACKUP_LATEST run. The generic run row
// carries status and timing; this carries what the backup itself did.
//
// The target fields are captured when the run is created and are never
// recomputed. A configuration change after the request must not be able to
// silently redirect a backup at a different disk or a different set of sources.
type BackupRun struct {
	RunID        string
	RepositoryID string
	DiskID       string
	// Semantics is always BACKUP_LATEST in v1. It is persisted so that a later
	// milestone adding another kind cannot silently reinterpret old rows.
	Semantics string
	Stage     DurabilityStage
	// SelectedSourceIDs is the immutable target captured at run creation. It is
	// the set of sources the administrator asked for, before any eligibility
	// check, so history shows what was requested as well as what happened.
	SelectedSourceIDs []string

	ReadSetCapturedAt   time.Time
	BarrierAcquiredAt   time.Time
	ResticStartedAt     time.Time
	CandidateSnapshotID string
	VerifiedSnapshotID  string
	SnapshotVerifiedAt  time.Time
	SyncedAt            time.Time
	UnmountVerifiedAt   time.Time
	DurableSuccessAt    time.Time
	Durability          BackupDurability

	FilesProcessed  int64
	FilesNew        int64
	FilesChanged    int64
	FilesUnmodified int64
	BytesProcessed  int64
	BytesAdded      int64
	EngineErrors    int64
}

// DurableSuccess reports whether this run reached a proven durable end.
func (b BackupRun) DurableSuccess() bool { return b.Stage == StageDurableSuccess }

// CandidateForRecovery reports whether the run left the repository in a state
// that an explicit recovery must resolve. It is true exactly when restic ran
// but the run never reached its durability boundary.
func (b BackupRun) CandidateForRecovery() bool {
	return b.Stage.RepositoryMayHaveChanged() && b.Stage != StageDurableSuccess
}

// BackupSourceState is the audit record of exactly which published staging
// state of one source was read by a backup run.
//
// It is written before restic starts, not derived afterwards. A record built
// after the fact could only say what the source looks like now, which is a
// different question from what was actually read.
type BackupSourceState struct {
	BackupRunID string
	SourceID    string
	// CurrentCollectionRunID is the collection run that published this state.
	CurrentCollectionRunID string
	StagingStateVersion    int64
	PublishedAt            time.Time
	// CurrentRelativePath is the staging-root-relative path that was read. It
	// is recorded so history can show the exact read-set without needing the
	// source configuration to still agree.
	CurrentRelativePath string
	CapturedAt          time.Time
}

// BackupRevisionBatch is the audit record of one revision batch included in a
// backup run's read-set, captured before restic starts.
type BackupRevisionBatch struct {
	BackupRunID     string
	RevisionBatchID string
	SourceID        string
	RelativePath    string
	BatchCreatedAt  time.Time
	CapturedAt      time.Time
}

// RevisionCoverage records that one revision batch is provably contained in one
// verified snapshot, on one repository, on one registered disk.
//
// Coverage is only ever written from an exact read-set that reached a proven
// durable end. It is deliberately not derived from timestamps: "this batch was
// created before restic started" does not establish that restic read it, and a
// retention decision built on that inference could delete the only copy of
// data that was never actually backed up.
type RevisionCoverage struct {
	RevisionBatchID string
	BackupRunID     string
	RepositoryID    string
	DiskID          string
	SnapshotID      string
	CoveredAt       time.Time
}
