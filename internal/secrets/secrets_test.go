package secrets

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	return store
}

func TestPutMaterializeDelete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	const password = "correct horse battery staple"

	locator, err := store.Put(ctx, domain.SecretPurposeSMBPassword, password)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if strings.Contains(locator, password) {
		t.Fatal("the locator must not embed the secret")
	}
	if len(locator) != 32 {
		t.Fatalf("locator = %q, want a 32 character generated identifier", locator)
	}

	var seen string
	if err := store.Materialize(ctx, locator, func(material domain.SecretMaterial) error {
		seen = material.Reveal()
		if material.String() != "[REDACTED]" {
			t.Fatal("material must redact itself when formatted")
		}
		return nil
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if seen != password {
		t.Fatalf("materialized %q, want the stored password", seen)
	}

	if err := store.Delete(ctx, locator); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Materialize(ctx, locator, func(domain.SecretMaterial) error { return nil }); err == nil {
		t.Fatal("materializing a deleted secret must fail")
	}
	// Deleting again is not an error.
	if err := store.Delete(ctx, locator); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPutRejectsEmptyAndOversizedValues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, domain.SecretPurposeSMBPassword, ""); err == nil {
		t.Fatal("an empty secret must be rejected")
	}
	if _, err := store.Put(ctx, domain.SecretPurposeSMBPassword, strings.Repeat("x", MaxSecretBytes+1)); err == nil {
		t.Fatal("an oversized secret must be rejected")
	}
}

func TestLocatorsAreNeverDerivedFromCallerInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()

	unsafe := []string{
		"../escape",
		"..",
		"/etc/passwd",
		`C:\Windows\System32\config\SAM`,
		"has spaces",
		"UPPERCASE0000000000000000000000",
		strings.Repeat("a", 31),
		"",
	}
	for _, locator := range unsafe {
		if err := store.Materialize(ctx, locator, func(domain.SecretMaterial) error { return nil }); err == nil {
			t.Fatalf("locator %q should have been refused", locator)
		}
		if err := store.Delete(ctx, locator); err == nil {
			t.Fatalf("deleting with locator %q should have been refused", locator)
		}
	}
}

func TestStoredSecretsAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not a Windows security boundary; production credential support is Linux-only in v1")
	}
	t.Parallel()

	store := newTestStore(t)
	locator, err := store.Put(context.Background(), domain.SecretPurposeSMBPassword, "a-password")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		t.Fatalf("stat secret root: %v", err)
	}
	if mode := rootInfo.Mode().Perm(); mode != DirMode {
		t.Fatalf("secret root mode = %o, want %o", mode, DirMode)
	}
	fileInfo, err := os.Stat(filepath.Join(store.Root(), locator+secretSuffix))
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != FileMode {
		t.Fatalf("secret file mode = %o, want %o", mode, FileMode)
	}
}

func TestRuntimeDirIsPrivateAndRemoved(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	dir, cleanup, err := store.NewRuntimeDir()
	if err != nil {
		t.Fatalf("new runtime dir: %v", err)
	}
	if !strings.HasPrefix(dir, store.Root()) {
		t.Fatalf("runtime dir %q must live under the secret root %q", dir, store.Root())
	}

	path, err := WriteRuntimeFile(dir, "rclone.conf", []byte("pass = obscured"))
	if err != nil {
		t.Fatalf("write runtime file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("runtime file was not written: %v", err)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the runtime directory must be removed after the operation (err=%v)", err)
	}
}

func TestWriteRuntimeFileRejectsPathSegments(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	dir, cleanup, err := store.NewRuntimeDir()
	if err != nil {
		t.Fatalf("new runtime dir: %v", err)
	}
	defer cleanup()

	for _, name := range []string{"../escape.conf", "nested/file.conf", `nested\file.conf`, "", ".", ".."} {
		if _, err := WriteRuntimeFile(dir, name, []byte("x")); err == nil {
			t.Fatalf("runtime file name %q should have been rejected", name)
		}
	}
}

func TestRedactorReplacesKnownValues(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor("super-secret", "obscured-form")
	text := "config error near pass = obscured-form for user with super-secret"
	got := redactor.Redact(text)

	if strings.Contains(got, "super-secret") || strings.Contains(got, "obscured-form") {
		t.Fatalf("redaction left a secret behind: %q", got)
	}
	if strings.Count(got, Placeholder) != 2 {
		t.Fatalf("redacted text = %q, want two placeholders", got)
	}

	// Very short values are ignored so diagnostics are not shredded.
	short := NewRedactor("ab")
	if got := short.Redact("a table of abbreviations"); got != "a table of abbreviations" {
		t.Fatalf("a two-character value must not be redacted, got %q", got)
	}
	// A nil redactor is a no-op rather than a panic.
	var absent *Redactor
	if got := absent.Redact("unchanged"); got != "unchanged" {
		t.Fatalf("nil redactor changed the text: %q", got)
	}
}

func TestNewFileStoreRejectsAnUnconfiguredRoot(t *testing.T) {
	t.Parallel()

	if _, err := NewFileStore("  "); err == nil {
		t.Fatal("an empty secret root must be rejected")
	}
}
