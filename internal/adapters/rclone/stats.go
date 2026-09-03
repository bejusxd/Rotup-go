package rclone

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/bejusxd/rotup/internal/ports"
)

// logLine is one record of rclone's --use-json-log output. Only the fields
// ROTUP acts on are decoded; everything else is ignored so a new rclone version
// adding fields cannot break parsing.
type logLine struct {
	Level  string       `json:"level"`
	Msg    string       `json:"msg"`
	Object string       `json:"object"`
	Stats  *statsRecord `json:"stats"`
}

type statsRecord struct {
	Bytes          int64 `json:"bytes"`
	Checks         int64 `json:"checks"`
	Deletes        int64 `json:"deletes"`
	DeletedDirs    int64 `json:"deletedDirs"`
	Errors         int64 `json:"errors"`
	FatalError     bool  `json:"fatalError"`
	Renames        int64 `json:"renames"`
	RetryError     bool  `json:"retryError"`
	TotalBytes     int64 `json:"totalBytes"`
	TotalChecks    int64 `json:"totalChecks"`
	TotalTransfers int64 `json:"totalTransfers"`
	Transfers      int64 `json:"transfers"`
}

// maxDiagnosticLines bounds how many engine messages are retained for history.
const maxDiagnosticLines = 20

// maxDiagnosticLineLength bounds each retained message.
const maxDiagnosticLineLength = 400

// parsedOutput is what the adapter extracts from one rclone invocation.
type parsedOutput struct {
	Stats       ports.TransferStats
	ErrorLines  []string
	NoticeLines []string
}

// parseLogOutput scans rclone's JSON log stream. The stream is a retained tail,
// so the first line may be a fragment; unparsable lines are skipped rather than
// treated as failures. The last record carrying a stats object wins, because
// rclone emits its final summary at the end of the run.
func parseLogOutput(raw string) parsedOutput {
	result := parsedOutput{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var record logLine
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Stats != nil {
			result.Stats = ports.TransferStats{
				Available:      true,
				Bytes:          record.Stats.Bytes,
				TotalBytes:     record.Stats.TotalBytes,
				Checks:         record.Stats.Checks,
				Transfers:      record.Stats.Transfers,
				TotalTransfers: record.Stats.TotalTransfers,
				Deletes:        record.Stats.Deletes + record.Stats.DeletedDirs,
				Renames:        record.Stats.Renames,
				Errors:         record.Stats.Errors,
				FatalError:     record.Stats.FatalError,
				RetryError:     record.Stats.RetryError,
			}
		}
		switch strings.ToLower(record.Level) {
		case "error", "critical", "fatal":
			if len(result.ErrorLines) < maxDiagnosticLines {
				result.ErrorLines = append(result.ErrorLines, formatDiagnostic(record))
			}
		case "warning":
			if len(result.NoticeLines) < maxDiagnosticLines {
				result.NoticeLines = append(result.NoticeLines, formatDiagnostic(record))
			}
		}
	}
	return result
}

func formatDiagnostic(record logLine) string {
	message := strings.TrimSpace(record.Msg)
	if record.Object != "" {
		message = record.Object + ": " + message
	}
	message = collapseWhitespace(message)
	if len(message) > maxDiagnosticLineLength {
		message = message[:maxDiagnosticLineLength] + "…"
	}
	return message
}

var whitespaceRun = regexp.MustCompile(`\s+`)

func collapseWhitespace(value string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(value, " "))
}

// sizeOutput is the machine-readable result of `rclone size --json`.
type sizeOutput struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

// listingEntry is one element of `rclone lsjson` output. Only the presence and
// count of entries matter for a connection test.
type listingEntry struct {
	Path  string `json:"Path"`
	IsDir bool   `json:"IsDir"`
}

// versionPattern extracts the semantic version from `rclone version` output,
// whose first line looks like "rclone v1.66.0".
var versionPattern = regexp.MustCompile(`rclone\s+v?([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

func parseVersion(output string) string {
	match := versionPattern.FindStringSubmatch(output)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}
