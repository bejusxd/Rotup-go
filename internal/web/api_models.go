package web

import (
	"encoding/json"
	"time"

	"github.com/bejusxd/rotup/internal/backup"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/removable"
)

// Explicit allowlisted wire models. Credential references/locators, raw engine
// output and arbitrary internal JSON never enter API responses.
type sourceDTO struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Host                string     `json:"host"`
	ShareName           string     `json:"shareName"`
	Subdirectory        string     `json:"subdirectory"`
	Username            string     `json:"username"`
	StagingRelativePath string     `json:"stagingRelativePath"`
	Enabled             bool       `json:"enabled"`
	IncludeRules        []string   `json:"includeRules"`
	ExcludeRules        []string   `json:"excludeRules"`
	Policy              string     `json:"policy"`
	Status              string     `json:"status"`
	StateVersion        int64      `json:"stateVersion"`
	HasCredential       bool       `json:"hasCredential"`
	Busy                bool       `json:"busy"`
	LastPublishedAt     *time.Time `json:"lastPublishedAt"`
	LastCollectionRunID string     `json:"lastCollectionRunId"`
}

func sourceInfo(s domain.Source, busy bool) sourceDTO {
	return sourceDTO{s.ID, s.Name, s.Host, s.ShareName, s.Subdirectory, s.Username,
		s.StagingRelativePath, s.Enabled, nonNil(s.IncludeRules), nonNil(s.ExcludeRules),
		string(s.ReconciliationPolicy), string(s.StagingStatus), s.StagingStateVersion,
		s.HasCredential(), busy, s.LastPublishedAt, s.LastCollectionRunID}
}

type runDTO struct {
	ID           string       `json:"id"`
	Kind         string       `json:"kind"`
	Trigger      string       `json:"trigger"`
	SourceID     string       `json:"sourceId"`
	RepositoryID string       `json:"repositoryId"`
	DiskID       string       `json:"diskId"`
	Status       string       `json:"status"`
	Terminal     bool         `json:"terminal"`
	Summary      string       `json:"summary"`
	Error        *apiErrorDTO `json:"error"`
	RequestedAt  *time.Time   `json:"requestedAt"`
	StartedAt    *time.Time   `json:"startedAt"`
	FinishedAt   *time.Time   `json:"finishedAt"`
	DurationMS   int64        `json:"durationMs"`
}

func runInfo(r domain.Run) runDTO {
	v := runDTO{ID: r.ID, Kind: string(r.Kind), Trigger: string(r.TriggerKind), SourceID: r.SourceID,
		Status: string(r.Status), Terminal: r.Status.Terminal(), Summary: r.Summary,
		RequestedAt: instant(r.RequestedAt), StartedAt: instant(r.StartedAt), FinishedAt: instant(r.FinishedAt), DurationMS: r.Duration().Milliseconds()}
	// These two operation kinds record their target in a structured summary.
	// Decode only known identifiers, never return the arbitrary summary map.
	if r.Kind == domain.RunKindDiskSetup || r.Kind == domain.RunKindVerify {
		var target struct {
			RepositoryID string `json:"repository_id"`
			DiskID       string `json:"disk_id"`
		}
		if json.Unmarshal([]byte(r.SummaryJSON), &target) == nil {
			v.RepositoryID, v.DiskID = target.RepositoryID, target.DiskID
		}
	}
	if r.ErrorCode != "" || r.SafeError != "" {
		v.Error = &apiErrorDTO{r.ErrorCode, string(r.ErrorClass), r.SafeError, userErrorMessage(string(r.ErrorClass), r.ErrorCode)}
	}
	return v
}

type diskDTO struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Registered      bool       `json:"registered"`
	Enabled         bool       `json:"enabled"`
	FilesystemUUID  string     `json:"filesystemUuid"`
	FilesystemType  string     `json:"filesystemType"`
	SizeBytes       int64      `json:"sizeBytes"`
	Status          string     `json:"status"`
	ErrorCode       string     `json:"errorCode"`
	Supported       bool       `json:"supported"`
	Ambiguous       bool       `json:"ambiguous"`
	Busy            bool       `json:"busy"`
	Observed        bool       `json:"observed"`
	LastConnectedAt *time.Time `json:"lastConnectedAt"`
	DevicePath      string     `json:"devicePath,omitempty"`
	MountPoint      string     `json:"mountPoint,omitempty"`
	ExternalMounts  []string   `json:"externalMounts,omitempty"`
}

func diskInfo(v removable.MediaView, debug bool) diskDTO {
	d := diskDTO{Registered: v.Registered(), Status: string(v.Status), ErrorCode: v.ErrorCode,
		Supported: v.Supported, Ambiguous: v.Ambiguous, Busy: v.Busy, Observed: v.Observed != nil}
	if v.Observed != nil {
		d.Name, d.FilesystemUUID, d.FilesystemType = v.Observed.Label, v.Observed.FilesystemUUID, v.Observed.FilesystemType
		d.SizeBytes = v.Observed.SizeBytes
		if debug {
			d.DevicePath = v.Observed.DevicePath
		}
	}
	if v.Disk != nil {
		d.ID, d.Name, d.Enabled = v.Disk.ID, v.Disk.Name, v.Disk.Enabled
		d.FilesystemUUID, d.FilesystemType, d.LastConnectedAt = v.Disk.FilesystemUUID, v.Disk.FilesystemType, v.Disk.LastConnectedAt
	}
	if debug {
		d.MountPoint, d.ExternalMounts = v.MountPoint, v.ExternalMounts
	}
	return d
}

type repositoryDTO struct {
	ID                     string     `json:"id"`
	DiskID                 string     `json:"diskId"`
	DiskName               string     `json:"diskName"`
	FilesystemUUID         string     `json:"filesystemUuid"`
	RelativePath           string     `json:"relativePath"`
	ResticID               string     `json:"resticId"`
	Health                 string     `json:"health"`
	Reason                 string     `json:"reason"`
	Established            bool       `json:"established"`
	Connected              bool       `json:"connected"`
	Busy                   bool       `json:"busy"`
	HasPassword            bool       `json:"hasPassword"`
	LastSnapshotID         string     `json:"lastSnapshotId"`
	LastSuccessfulBackupAt *time.Time `json:"lastSuccessfulBackupAt"`
	EstablishedAt          *time.Time `json:"establishedAt"`
}

func repositoryInfo(v backup.RepositoryView) repositoryDTO {
	r := v.Repository
	d := repositoryDTO{r.ID, r.DiskID, "", r.FilesystemUUID, r.RelativePath, r.ResticRepositoryID,
		string(r.Health), r.SuspectReason, r.Established(), v.Connected, v.Busy, v.HasPassword,
		r.LastSnapshotID, r.LastSuccessfulBackupAt, r.EstablishedAt}
	if v.Disk != nil {
		d.DiskName = v.Disk.Name
	}
	return d
}

type sourceResultDTO struct {
	StateVersion           int64      `json:"stateVersion"`
	PublishedAt            *time.Time `json:"publishedAt"`
	BytesScanned           int64      `json:"bytesScanned"`
	BytesCopied            int64      `json:"bytesCopied"`
	FilesScanned           int64      `json:"filesScanned"`
	FilesCopied            int64      `json:"filesCopied"`
	FilesDeletedOrArchived int64      `json:"filesDeletedOrArchived"`
	EngineErrors           int64      `json:"engineErrors"`
	Warnings               []string   `json:"warnings"`
}

type revisionDTO struct {
	ID                      string     `json:"id"`
	SourceID                string     `json:"sourceId"`
	CollectionRunID         string     `json:"collectionRunId"`
	RelativePath            string     `json:"relativePath"`
	CreatedAt               *time.Time `json:"createdAt"`
	ItemCount               int64      `json:"itemCount"`
	ByteCount               int64      `json:"byteCount"`
	ReconciliationCompleted bool       `json:"reconciliationCompleted"`
}

func revisionInfo(b domain.RevisionBatch) revisionDTO {
	return revisionDTO{b.ID, b.SourceID, b.CollectionRunID, b.RelativePath, instant(b.CreatedAt), b.ItemCount, b.ByteCount, b.ReconciliationCompleted}
}

type backupDTO struct {
	RepositoryID        string         `json:"repositoryId"`
	DiskID              string         `json:"diskId"`
	Semantics           string         `json:"semantics"`
	Stage               string         `json:"stage"`
	Durability          string         `json:"durability"`
	SelectedSourceIDs   []string       `json:"selectedSourceIds"`
	CandidateSnapshotID string         `json:"candidateSnapshotId"`
	VerifiedSnapshotID  string         `json:"verifiedSnapshotId"`
	ResticStartedAt     *time.Time     `json:"resticStartedAt"`
	SyncedAt            *time.Time     `json:"syncedAt"`
	UnmountVerifiedAt   *time.Time     `json:"unmountVerifiedAt"`
	DurableSuccessAt    *time.Time     `json:"durableSuccessAt"`
	BytesProcessed      int64          `json:"bytesProcessed"`
	BytesAdded          int64          `json:"bytesAdded"`
	FilesProcessed      int64          `json:"filesProcessed"`
	EngineErrors        int64          `json:"engineErrors"`
	ReadSet             []readStateDTO `json:"readSet"`
	RevisionBatches     []readBatchDTO `json:"revisionBatches"`
}
type readStateDTO struct {
	SourceID        string     `json:"sourceId"`
	CollectionRunID string     `json:"collectionRunId"`
	StateVersion    int64      `json:"stateVersion"`
	RelativePath    string     `json:"relativePath"`
	PublishedAt     *time.Time `json:"publishedAt"`
	CapturedAt      *time.Time `json:"capturedAt"`
}
type readBatchDTO struct {
	ID           string     `json:"id"`
	SourceID     string     `json:"sourceId"`
	RelativePath string     `json:"relativePath"`
	CreatedAt    *time.Time `json:"createdAt"`
	CapturedAt   *time.Time `json:"capturedAt"`
}

func backupInfo(b domain.BackupRun) backupDTO {
	return backupDTO{RepositoryID: b.RepositoryID, DiskID: b.DiskID, Semantics: b.Semantics, Stage: string(b.Stage), Durability: string(b.Durability),
		SelectedSourceIDs: nonNil(b.SelectedSourceIDs), CandidateSnapshotID: b.CandidateSnapshotID, VerifiedSnapshotID: b.VerifiedSnapshotID,
		ResticStartedAt: instant(b.ResticStartedAt), SyncedAt: instant(b.SyncedAt), UnmountVerifiedAt: instant(b.UnmountVerifiedAt), DurableSuccessAt: instant(b.DurableSuccessAt),
		BytesProcessed: b.BytesProcessed, BytesAdded: b.BytesAdded, FilesProcessed: b.FilesProcessed, EngineErrors: b.EngineErrors,
		ReadSet: []readStateDTO{}, RevisionBatches: []readBatchDTO{}}
}

func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
