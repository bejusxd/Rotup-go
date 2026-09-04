package domain

// Snapshot tags are the durable correlation contract, not adapter internals.
const (
	TagROTUP          = "rotup"
	TagBackupLatest   = "rotup-semantics:BACKUP_LATEST"
	TagSnapshotSchema = "rotup-schema:1"
)

func RunTag(runID string) string               { return "rotup-run:" + runID }
func RepositoryTag(repositoryID string) string { return "rotup-repo:" + repositoryID }
func DiskTag(diskID string) string             { return "rotup-disk:" + diskID }
