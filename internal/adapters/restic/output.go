package restic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/ports"
)

// parseVersion reads the version out of `restic version` output, which is a
// single line of the form "restic 0.17.3 compiled with go1.23.3 on linux/amd64".
func parseVersion(text string) string {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "restic") {
			candidate := strings.TrimSpace(fields[1])
			if candidate != "" && (candidate[0] >= '0' && candidate[0] <= '9') {
				return candidate
			}
		}
	}
	return ""
}

// repositoryConfig is the shape of `restic cat config`, which is the
// authoritative statement of a repository's identity. It is read back from the
// repository rather than inferred from where the repository was found.
type repositoryConfig struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// parseRepositoryConfig decodes the repository configuration document. The
// output is a single JSON object, so a stray banner line before it is skipped
// rather than allowed to break decoding.
func parseRepositoryConfig(raw []byte) (ports.RepositoryInfo, bool) {
	trimmed := trimToJSONObject(raw)
	var config repositoryConfig
	if err := json.Unmarshal(bytes.TrimSpace(trimmed), &config); err != nil {
		return ports.RepositoryInfo{}, false
	}
	if config.ID == "" {
		return ports.RepositoryInfo{}, false
	}
	return ports.RepositoryInfo{ID: config.ID, Version: config.Version}, true
}

// backupMessage is one line of `restic backup --json`. restic streams several
// message types on stdout; only the terminal summary establishes that a
// snapshot was written, and only error messages establish that it was not what
// was asked for.
type backupMessage struct {
	MessageType string `json:"message_type"`
	// SnapshotID appears on the summary message.
	SnapshotID string `json:"snapshot_id"`
	// Counters from the summary message.
	FilesNew            int64 `json:"files_new"`
	FilesChanged        int64 `json:"files_changed"`
	FilesUnmodified     int64 `json:"files_unmodified"`
	TotalFilesProcessed int64 `json:"total_files_processed"`
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
	DataAdded           int64 `json:"data_added"`
	DataAddedPacked     int64 `json:"data_added_packed"`
	// Error and During appear on error messages.
	Error  json.RawMessage `json:"error"`
	During string          `json:"during"`
	Item   string          `json:"item"`
	// Struct fields restic uses on status messages, decoded only so that a
	// status line is recognized rather than misread.
	PercentDone float64 `json:"percent_done"`
}

// parseBackupOutput reads restic's structured backup output.
//
// It deliberately reports counters only when the terminal summary was found.
// Statistics assembled from partial output would let a truncated or interrupted
// stream look like a completed backup, and the whole point of parsing this is
// to know whether restic finished what it started.
func parseBackupOutput(raw []byte) ports.BackupStats {
	var stats ports.BackupStats
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLine)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var message backupMessage
		if err := json.Unmarshal(line, &message); err != nil {
			continue
		}
		switch message.MessageType {
		case "error":
			stats.Errors++
		case "summary":
			stats.Available = true
			stats.SnapshotID = strings.ToLower(strings.TrimSpace(message.SnapshotID))
			stats.FilesNew = message.FilesNew
			stats.FilesChanged = message.FilesChanged
			stats.FilesUnmodified = message.FilesUnmodified
			stats.FilesProcessed = message.TotalFilesProcessed
			stats.BytesProcessed = message.TotalBytesProcessed
			stats.BytesAdded = message.DataAdded
		}
	}
	return stats
}

// snapshotMessage is one entry of `restic snapshots --json`.
type snapshotMessage struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Time     string   `json:"time"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
	Hostname string   `json:"hostname"`
}

// parseSnapshots decodes the snapshot listing. A listing that cannot be decoded
// is reported as such rather than as an empty repository: "no snapshots" and
// "the answer could not be read" must never look the same to a verifier.
func parseSnapshots(raw []byte) ([]ports.Snapshot, bool) {
	trimmed := bytes.TrimSpace(trimToJSONArray(raw))
	if len(trimmed) == 0 {
		return nil, false
	}
	var messages []snapshotMessage
	if err := json.Unmarshal(trimmed, &messages); err != nil {
		return nil, false
	}
	result := make([]ports.Snapshot, 0, len(messages))
	for _, message := range messages {
		snapshot := ports.Snapshot{
			ID:       strings.ToLower(strings.TrimSpace(message.ID)),
			ShortID:  strings.ToLower(strings.TrimSpace(message.ShortID)),
			Tags:     message.Tags,
			Paths:    message.Paths,
			Hostname: message.Hostname,
		}
		if parsed, err := time.Parse(time.RFC3339Nano, message.Time); err == nil {
			snapshot.Time = parsed.UTC()
		}
		result = append(result, snapshot)
	}
	return result, true
}

// checkMessage is one line of `restic check --json`. Older restic releases emit
// plain text instead, which countCheckErrors falls back to.
type checkMessage struct {
	MessageType string `json:"message_type"`
	Message     string `json:"message"`
}

// countCheckErrors counts the problems a repository check reported in restic's
// structured output.
//
// It is deliberately additive detail, not the verdict. Whether a check passed
// is decided by restic's exit status, which is non-zero when it finds problems.
// Counting error-looking lines in restic's human output instead would be
// unsound in both directions: restic 0.17 ignores --json for check and prints
// the plain line "no errors were found" on success, which any such heuristic
// reads as a failure. A verifier that fails on a healthy repository is not
// being careful, it is being wrong, and it would make every recovery
// impossible to complete.
func countCheckErrors(raw []byte) int64 {
	var errorsFound int64

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var message checkMessage
		if err := json.Unmarshal(line, &message); err == nil && message.MessageType == "error" {
			errorsFound++
		}
	}
	return errorsFound
}

// maxJSONLine bounds one structured output line so a pathological stream cannot
// make the parser allocate without limit.
const maxJSONLine = 1024 * 1024

// trimToJSONObject discards anything before the first "{" so a stray banner
// line cannot break decoding of a single JSON object.
func trimToJSONObject(raw []byte) []byte {
	index := bytes.IndexByte(raw, '{')
	if index <= 0 {
		return raw
	}
	return raw[index:]
}

// trimToJSONArray discards anything before the first "[" for the same reason.
func trimToJSONArray(raw []byte) []byte {
	index := bytes.IndexByte(raw, '[')
	if index <= 0 {
		return raw
	}
	return raw[index:]
}

// formatCount renders a counter for a safe diagnostic message.
func formatCount(value int64) string { return strconv.FormatInt(value, 10) }
