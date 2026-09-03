package domain

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
)

// ReconciliationPolicy selects how a collection run treats staging objects that
// the source no longer has, or has replaced.
type ReconciliationPolicy string

const (
	// PolicyCopyOnly copies additions and changes and never removes anything
	// from the current tree.
	PolicyCopyOnly ReconciliationPolicy = "copy_only"
	// PolicyArchiveThenSync moves displaced and source-deleted objects into a
	// run-stamped revision batch before the current tree is reconciled.
	PolicyArchiveThenSync ReconciliationPolicy = "archive_then_sync"
	// PolicyMirror propagates deletions destructively. It is defined by the
	// architecture but is not implemented in M2 and is rejected explicitly.
	PolicyMirror ReconciliationPolicy = "mirror"
)

// SupportedPolicies lists the policies a collection run may actually execute.
var SupportedPolicies = []ReconciliationPolicy{PolicyArchiveThenSync, PolicyCopyOnly}

// Supported reports whether a collection run may execute this policy.
func (p ReconciliationPolicy) Supported() bool {
	return p == PolicyCopyOnly || p == PolicyArchiveThenSync
}

// ArchivesDisplaced reports whether the policy must preserve replaced and
// deleted staging objects in a revision batch.
func (p ReconciliationPolicy) ArchivesDisplaced() bool { return p == PolicyArchiveThenSync }

// StagingStatus is the durable claim ROTUP makes about a source's current tree.
type StagingStatus string

const (
	// StagingEmpty means nothing has ever been published for the source.
	StagingEmpty StagingStatus = "EMPTY"
	// StagingUpdating means reconciliation may currently be mutating current.
	StagingUpdating StagingStatus = "UPDATING"
	// StagingStable means current holds a completely reconciled published view.
	StagingStable StagingStatus = "STABLE"
	// StagingRecoveryRequired means ROTUP cannot prove current is coherent.
	StagingRecoveryRequired StagingStatus = "RECOVERY_REQUIRED"
)

// Source is the M2 subset of the configured network source.
type Source struct {
	ID                   string
	Name                 string
	Host                 string
	ShareName            string
	Subdirectory         string
	Username             string
	CredentialRefID      string
	StagingRelativePath  string
	Enabled              bool
	IncludeRules         []string
	ExcludeRules         []string
	ReconciliationPolicy ReconciliationPolicy
	StagingStatus        StagingStatus
	StagingStateVersion  int64
	LastPublishedAt      *time.Time
	LastCollectionRunID  string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// HasCredential reports whether a stored password is associated with the source.
func (s Source) HasCredential() bool { return s.CredentialRefID != "" }

// RemotePath returns the share-relative path that identifies the collected
// subtree. It is a semantic value, not a prebuilt UNC or rclone argument.
func (s Source) RemotePath() string {
	if s.Subdirectory == "" {
		return s.ShareName
	}
	return s.ShareName + "/" + s.Subdirectory
}

const (
	maxNameLength    = 120
	maxHostLength    = 253
	maxPathLength    = 200
	maxRuleCount     = 64
	maxRuleLength    = 200
	maxSegmentLength = 100
)

// ValidateSource checks every administrator-supplied field. existingPaths maps
// other sources' normalized staging paths to their source ID so overlapping
// destinations can be rejected.
func ValidateSource(s *Source, existingPaths map[string]string) error {
	if err := validateDisplayName(s.Name); err != nil {
		return err
	}
	s.Name = strings.TrimSpace(s.Name)

	host, err := validateHost(s.Host)
	if err != nil {
		return err
	}
	s.Host = host

	share, err := validateShareName(s.ShareName)
	if err != nil {
		return err
	}
	s.ShareName = share

	subdirectory, err := NormalizeRelativePath(s.Subdirectory, "subdirectory")
	if err != nil {
		return err
	}
	s.Subdirectory = subdirectory

	s.Username = strings.TrimSpace(s.Username)
	if strings.ContainsAny(s.Username, "\r\n\x00") {
		return NewError(ErrClassConfiguration, "invalid_username", "username must not contain control characters")
	}
	if len(s.Username) > maxNameLength {
		return NewError(ErrClassConfiguration, "invalid_username", "username is too long")
	}

	stagingPath, err := NormalizeRelativePath(s.StagingRelativePath, "staging destination")
	if err != nil {
		return err
	}
	if stagingPath == "" {
		return NewError(ErrClassConfiguration, "invalid_staging_path", "staging destination must not be empty")
	}
	s.StagingRelativePath = stagingPath
	if err := checkStagingOverlap(s.ID, stagingPath, existingPaths); err != nil {
		return err
	}

	if !s.ReconciliationPolicy.Supported() {
		if s.ReconciliationPolicy == PolicyMirror {
			return NewError(ErrClassConfiguration, "policy_unsupported",
				"reconciliation policy \"mirror\" is defined by the architecture but is not implemented yet; choose archive_then_sync or copy_only")
		}
		return NewError(ErrClassConfiguration, "policy_unknown",
			fmt.Sprintf("unknown reconciliation policy %q", string(s.ReconciliationPolicy)))
	}

	if s.IncludeRules, err = validateRules(s.IncludeRules, "include"); err != nil {
		return err
	}
	if s.ExcludeRules, err = validateRules(s.ExcludeRules, "exclude"); err != nil {
		return err
	}
	return nil
}

func validateDisplayName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return NewError(ErrClassConfiguration, "invalid_name", "name must not be empty")
	}
	if len(trimmed) > maxNameLength {
		return NewError(ErrClassConfiguration, "invalid_name", "name is too long")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return NewError(ErrClassConfiguration, "invalid_name", "name must not contain control characters")
		}
	}
	return nil
}

func validateHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", NewError(ErrClassConfiguration, "invalid_host", "SMB host must not be empty")
	}
	if len(host) > maxHostLength {
		return "", NewError(ErrClassConfiguration, "invalid_host", "SMB host is too long")
	}
	// Reject anything that could be read as a flag, a URL, a path, or a second
	// argument once the adapter builds its argument array.
	if strings.HasPrefix(host, "-") {
		return "", NewError(ErrClassConfiguration, "invalid_host", "SMB host must not start with \"-\"")
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return "", NewError(ErrClassConfiguration, "invalid_host",
				"SMB host may only contain letters, digits, \".\", \"-\", \"_\" and \":\"")
		}
	}
	return host, nil
}

func validateShareName(raw string) (string, error) {
	share := strings.TrimSpace(raw)
	share = strings.Trim(share, "/\\")
	if share == "" {
		return "", NewError(ErrClassConfiguration, "invalid_share", "SMB share must not be empty")
	}
	if len(share) > maxSegmentLength {
		return "", NewError(ErrClassConfiguration, "invalid_share", "SMB share is too long")
	}
	if strings.HasPrefix(share, "-") {
		return "", NewError(ErrClassConfiguration, "invalid_share", "SMB share must not start with \"-\"")
	}
	if strings.ContainsAny(share, "/\\:") || share == "." || share == ".." {
		return "", NewError(ErrClassConfiguration, "invalid_share", "SMB share must be a single share name")
	}
	for _, r := range share {
		if unicode.IsControl(r) {
			return "", NewError(ErrClassConfiguration, "invalid_share", "SMB share must not contain control characters")
		}
	}
	return share, nil
}

// NormalizeRelativePath validates and normalizes an administrator-supplied
// relative path. It rejects absolute paths, drive letters, traversal, UNC
// prefixes, and control characters. An empty input normalizes to "".
func NormalizeRelativePath(raw, field string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > maxPathLength {
		return "", NewError(ErrClassConfiguration, "invalid_path", field+" is too long")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", NewError(ErrClassConfiguration, "invalid_path", field+" must not contain control characters")
		}
	}
	// Normalize Windows separators before any structural check so that the same
	// rules apply regardless of how the administrator typed the path.
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.HasPrefix(value, "/") {
		return "", NewError(ErrClassConfiguration, "invalid_path", field+" must be relative, not absolute")
	}
	if len(value) >= 2 && value[1] == ':' {
		return "", NewError(ErrClassConfiguration, "invalid_path", field+" must not contain a drive letter")
	}

	cleaned := path.Clean(value)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", NewError(ErrClassConfiguration, "invalid_path", field+" must not traverse outside its root")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", NewError(ErrClassConfiguration, "invalid_path", field+" contains an invalid path segment")
		}
		if len(segment) > maxSegmentLength {
			return "", NewError(ErrClassConfiguration, "invalid_path", field+" contains an over-long path segment")
		}
		if strings.HasPrefix(segment, "-") {
			return "", NewError(ErrClassConfiguration, "invalid_path", field+" segments must not start with \"-\"")
		}
	}
	return cleaned, nil
}

func checkStagingOverlap(sourceID, stagingPath string, existingPaths map[string]string) error {
	for otherPath, otherID := range existingPaths {
		if otherID == sourceID {
			continue
		}
		if pathsOverlap(stagingPath, otherPath) {
			return NewError(ErrClassConfiguration, "staging_overlap",
				fmt.Sprintf("staging destination %q overlaps the destination %q already used by another source",
					stagingPath, otherPath))
		}
	}
	return nil
}

// pathsOverlap reports whether two normalized relative paths address the same
// subtree, either identically or as ancestor and descendant.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validateRules(rules []string, kind string) ([]string, error) {
	if len(rules) > maxRuleCount {
		return nil, NewError(ErrClassConfiguration, "invalid_rules",
			fmt.Sprintf("at most %d %s rules are supported", maxRuleCount, kind))
	}
	result := make([]string, 0, len(rules))
	for _, rule := range rules {
		trimmed := strings.TrimSpace(rule)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > maxRuleLength {
			return nil, NewError(ErrClassConfiguration, "invalid_rules", kind+" rule is too long")
		}
		if strings.HasPrefix(trimmed, "-") {
			return nil, NewError(ErrClassConfiguration, "invalid_rules",
				kind+" rule must not start with \"-\" because it would be read as an engine flag")
		}
		for _, r := range trimmed {
			if unicode.IsControl(r) {
				return nil, NewError(ErrClassConfiguration, "invalid_rules",
					kind+" rule must not contain control characters")
			}
		}
		result = append(result, trimmed)
	}
	return result, nil
}
