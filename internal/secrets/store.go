// Package secrets implements the v1 SecretStore: plaintext material lives in a
// dedicated service-owned directory outside SQLite, staging, and any web root,
// and is only ever handed to a callback for the duration of one operation.
//
// Linux is the production target, where the directory is created 0700 and files
// 0600 and the host filesystem permissions are the protection boundary. On
// Windows the same modes are requested so development and tests work, but
// POSIX mode bits are not a Windows security guarantee: Windows production
// credential support is explicitly out of scope for v1.
package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
)

const (
	// DirMode and FileMode are the modes requested on every secret path.
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600

	secretSuffix   = ".secret"
	runtimeDirName = "runtime"
	// MaxSecretBytes bounds stored material so a mistaken upload cannot fill
	// the secret volume.
	MaxSecretBytes = 4096
)

// FileStore stores each secret under an opaque generated identifier. File names
// are never derived from a source name, host, or any other user-supplied value.
type FileStore struct {
	root string
}

// NewFileStore prepares the secret root directory.
func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, domain.NewError(domain.ErrClassConfiguration, "secret_root_empty",
			"secret root must be configured")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "secret_root_invalid",
			"secret root could not be resolved", err)
	}
	if err := os.MkdirAll(absolute, DirMode); err != nil {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "secret_root_unusable",
			"secret root could not be created", err)
	}
	// MkdirAll leaves an existing directory's mode alone, so tighten it here.
	if err := os.Chmod(absolute, DirMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, domain.WrapError(domain.ErrClassConfiguration, "secret_root_unusable",
			"secret root permissions could not be set", err)
	}
	return &FileStore{root: absolute}, nil
}

// Provider names this store for credential_refs metadata.
func (s *FileStore) Provider() string { return domain.SecretProviderFile }

// Root returns the resolved secret root. It is used for health reporting and
// must never be shown next to secret material.
func (s *FileStore) Root() string { return s.root }

// Put writes new material and returns its opaque locator.
func (s *FileStore) Put(_ context.Context, purpose domain.SecretPurpose, value string) (string, error) {
	if value == "" {
		return "", domain.NewError(domain.ErrClassConfiguration, "secret_empty",
			"secret value must not be empty")
	}
	if len(value) > MaxSecretBytes {
		return "", domain.NewError(domain.ErrClassConfiguration, "secret_too_large",
			fmt.Sprintf("secret value must be at most %d bytes", MaxSecretBytes))
	}
	locator, err := randomLocator()
	if err != nil {
		return "", err
	}
	path, err := s.pathFor(locator)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, []byte(value)); err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "secret_write_failed",
			fmt.Sprintf("secret for purpose %s could not be stored", purpose), err)
	}
	return locator, nil
}

// Materialize reads the stored material and passes it to fn. The plaintext is
// never returned to the caller and is not retained after fn returns.
func (s *FileStore) Materialize(_ context.Context, locator string, fn func(domain.SecretMaterial) error) error {
	path, err := s.pathFor(locator)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.WrapError(domain.ErrClassConfiguration, "secret_missing",
				"the stored credential is missing; replace it before running this operation", err)
		}
		return domain.WrapError(domain.ErrClassInternal, "secret_read_failed",
			"the stored credential could not be read", err)
	}
	material := domain.NewSecretMaterial(string(raw))
	// Overwriting the read buffer limits how long plaintext stays in a
	// separately reachable allocation. Go's garbage collector still holds the
	// string copy, so this is defence in depth, not a guarantee.
	for i := range raw {
		raw[i] = 0
	}
	return fn(material)
}

// Delete removes stored material. A locator that is already gone is not an error.
func (s *FileStore) Delete(_ context.Context, locator string) error {
	path, err := s.pathFor(locator)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return domain.WrapError(domain.ErrClassInternal, "secret_delete_failed",
			"the stored credential could not be removed", err)
	}
	return nil
}

// NewRuntimeDir creates a private volatile directory for material an external
// tool needs as a file. It lives under the secret root, never in staging or on
// removable media, and the returned cleanup removes it.
func (s *FileStore) NewRuntimeDir() (string, func(), error) {
	base := filepath.Join(s.root, runtimeDirName)
	if err := os.MkdirAll(base, DirMode); err != nil {
		return "", nil, domain.WrapError(domain.ErrClassInternal, "runtime_dir_failed",
			"the private runtime directory could not be created", err)
	}
	name, err := randomLocator()
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, DirMode); err != nil {
		return "", nil, domain.WrapError(domain.ErrClassInternal, "runtime_dir_failed",
			"the private runtime directory could not be created", err)
	}
	cleanup := func() {
		// RemoveAll does not follow symlinks, so a planted link cannot redirect
		// this deletion outside the runtime directory.
		_ = os.RemoveAll(dir)
	}
	return dir, cleanup, nil
}

// WriteRuntimeFile writes transient material into a runtime directory created
// by NewRuntimeDir, with restrictive permissions.
func WriteRuntimeFile(dir, name string, contents []byte) (string, error) {
	if strings.ContainsAny(name, `/\`) || name == "" || name == "." || name == ".." {
		return "", domain.NewError(domain.ErrClassInvariant, "runtime_file_name",
			"runtime file name must be a single path segment")
	}
	path := filepath.Join(dir, name)
	if err := writeFileAtomic(path, contents); err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "runtime_file_failed",
			"transient engine material could not be written", err)
	}
	return path, nil
}

func (s *FileStore) pathFor(locator string) (string, error) {
	if !validLocator(locator) {
		return "", domain.NewError(domain.ErrClassInvariant, "secret_locator_invalid",
			"credential locator is not a value ROTUP generated")
	}
	return filepath.Join(s.root, locator+secretSuffix), nil
}

// validLocator only accepts the 32-hex-character form Put generates, which
// keeps any user-influenced value from reaching a filesystem path.
func validLocator(locator string) bool {
	if len(locator) != 32 {
		return false
	}
	for _, r := range locator {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func randomLocator() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", domain.WrapError(domain.ErrClassInternal, "secret_locator_failed",
			"a credential locator could not be generated", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// writeFileAtomic creates the file exclusively with restrictive permissions,
// flushes it, and renames it into place so a crash cannot leave a half-written
// secret behind.
func writeFileAtomic(path string, contents []byte) error {
	dir := filepath.Dir(path)
	temporaryName, err := randomLocator()
	if err != nil {
		return err
	}
	temporary := filepath.Join(dir, "."+temporaryName+".tmp")

	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()

	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Windows cannot rename onto an existing file, so clear the target first.
	// A crash between these two steps loses the old value; callers always write
	// a fresh locator rather than replacing one in place, so this cannot lose a
	// secret that is still referenced.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	removeTemporary = false
	// Requested explicitly because the umask may have loosened the create mode.
	if err := os.Chmod(path, FileMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	syncDir(dir)
	return nil
}
