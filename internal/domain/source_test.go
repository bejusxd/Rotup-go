package domain

import (
	"strings"
	"testing"
)

func validSource() Source {
	return Source{
		ID:                   "11111111-1111-4111-8111-111111111111",
		Name:                 "documents",
		Host:                 "fileserver.example",
		ShareName:            "docs",
		StagingRelativePath:  "documents",
		Enabled:              true,
		ReconciliationPolicy: PolicyArchiveThenSync,
	}
}

func TestValidateSourceAcceptsAWellFormedSource(t *testing.T) {
	t.Parallel()

	source := validSource()
	source.Subdirectory = "reports\\2026"
	if err := ValidateSource(&source, nil); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if source.Subdirectory != "reports/2026" {
		t.Fatalf("subdirectory = %q, want it normalized to forward slashes", source.Subdirectory)
	}
	if source.RemotePath() != "docs/reports/2026" {
		t.Fatalf("remote path = %q", source.RemotePath())
	}
}

func TestValidateSourceRejectsUnsafeStagingDestinations(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"parent traversal":        "../escape",
		"embedded traversal":      "documents/../../escape",
		"absolute posix path":     "/etc/rotup",
		"absolute windows path":   `C:\rotup`,
		"windows share path":      `\\server\share`,
		"empty destination":       "   ",
		"flag-like segment":       "--config",
		"traversal after cleanup": "a/b/../../..",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			source := validSource()
			source.StagingRelativePath = path
			if err := ValidateSource(&source, nil); err == nil {
				t.Fatalf("staging destination %q should have been rejected", path)
			}
		})
	}
}

func TestValidateSourceRejectsOverlappingStagingDestinations(t *testing.T) {
	t.Parallel()

	existing := map[string]string{"team/documents": "22222222-2222-4222-8222-222222222222"}
	cases := map[string]string{
		"identical":            "team/documents",
		"nested below another": "team/documents/reports",
		"parent of another":    "team",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			source := validSource()
			source.StagingRelativePath = path
			err := ValidateSource(&source, existing)
			if err == nil {
				t.Fatalf("overlapping destination %q should have been rejected", path)
			}
			var typed *Error
			if !AsError(err, &typed) || typed.Code != "staging_overlap" {
				t.Fatalf("error = %v, want a staging_overlap rejection", err)
			}
		})
	}
}

func TestValidateSourceAllowsASourceToKeepItsOwnDestination(t *testing.T) {
	t.Parallel()

	source := validSource()
	existing := map[string]string{"documents": source.ID}
	if err := ValidateSource(&source, existing); err != nil {
		t.Fatalf("a source must be allowed to keep its own destination: %v", err)
	}
}

func TestValidateSourceRejectsUnsupportedPolicies(t *testing.T) {
	t.Parallel()

	source := validSource()
	source.ReconciliationPolicy = PolicyMirror
	err := ValidateSource(&source, nil)
	if err == nil {
		t.Fatal("the mirror policy must be rejected while it is unimplemented")
	}
	var typed *Error
	if !AsError(err, &typed) || typed.Code != "policy_unsupported" {
		t.Fatalf("error = %v, want policy_unsupported", err)
	}
	if !strings.Contains(typed.SafeMessage(), "mirror") {
		t.Fatalf("the rejection must name the policy: %q", typed.SafeMessage())
	}

	source.ReconciliationPolicy = ReconciliationPolicy("something_else")
	if err := ValidateSource(&source, nil); err == nil {
		t.Fatal("an unknown policy must be rejected")
	}
}

func TestValidateSourceRejectsUnsafeSMBFields(t *testing.T) {
	t.Parallel()

	t.Run("host", func(t *testing.T) {
		for _, host := range []string{"", "   ", "-flag", "server /extra", "server\nname", "http://server/x"} {
			source := validSource()
			source.Host = host
			if err := ValidateSource(&source, nil); err == nil {
				t.Fatalf("host %q should have been rejected", host)
			}
		}
	})
	t.Run("share", func(t *testing.T) {
		for _, share := range []string{"", "..", "docs/extra", `docs\extra`, "-flag", "a:b"} {
			source := validSource()
			source.ShareName = share
			if err := ValidateSource(&source, nil); err == nil {
				t.Fatalf("share %q should have been rejected", share)
			}
		}
	})
	t.Run("subdirectory", func(t *testing.T) {
		for _, sub := range []string{"../escape", "/absolute", `C:\x`} {
			source := validSource()
			source.Subdirectory = sub
			if err := ValidateSource(&source, nil); err == nil {
				t.Fatalf("subdirectory %q should have been rejected", sub)
			}
		}
	})
}

func TestValidateSourceRejectsFlagLikeFilterRules(t *testing.T) {
	t.Parallel()

	source := validSource()
	source.ExcludeRules = []string{"--delete-during"}
	if err := ValidateSource(&source, nil); err == nil {
		t.Fatal("a rule that would be read as an engine flag must be rejected")
	}

	source = validSource()
	source.IncludeRules = []string{"  *.pdf  ", "", "  "}
	if err := ValidateSource(&source, nil); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	if len(source.IncludeRules) != 1 || source.IncludeRules[0] != "*.pdf" {
		t.Fatalf("include rules = %#v, want the blank entries dropped and values trimmed", source.IncludeRules)
	}
}

func TestNormalizeRelativePathNormalizesAndRejects(t *testing.T) {
	t.Parallel()

	accepted := map[string]string{
		"a/b":       "a/b",
		`a\b`:       "a/b",
		"./a/b":     "a/b",
		"a/./b":     "a/b",
		"a/b/c/../": "a/b",
		"":          "",
		"  ":        "",
		".":         "",
	}
	for input, want := range accepted {
		got, err := NormalizeRelativePath(input, "path")
		if err != nil {
			t.Fatalf("NormalizeRelativePath(%q) failed: %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeRelativePath(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"..", "../x", "/x", `C:\x`, "a/../../b", "a\x00b"} {
		if _, err := NormalizeRelativePath(input, "path"); err == nil {
			t.Fatalf("NormalizeRelativePath(%q) should have been rejected", input)
		}
	}
}

func TestIsIDAcceptsOnlyGeneratedIdentifiers(t *testing.T) {
	t.Parallel()

	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if !IsID(id) {
		t.Fatalf("generated identifier %q was rejected", id)
	}
	for _, value := range []string{"", "..", "../escape", "not-a-uuid", strings.Repeat("a", 36)} {
		if IsID(value) {
			t.Fatalf("IsID(%q) should be false", value)
		}
	}
}

func TestSecretMaterialRedactsItself(t *testing.T) {
	t.Parallel()

	material := NewSecretMaterial("super-secret")
	if got := material.String(); got != "[REDACTED]" {
		t.Fatalf("String() = %q", got)
	}
	if got := material.GoString(); got != "[REDACTED]" {
		t.Fatalf("GoString() = %q", got)
	}
	if got := material.LogValue().String(); got != "[REDACTED]" {
		t.Fatalf("LogValue() = %q", got)
	}
	if material.Reveal() != "super-secret" {
		t.Fatal("Reveal must return the plaintext")
	}
}
