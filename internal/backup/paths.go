package backup

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
)

// repositoryDirectory turns a verified mount point plus ROTUP's own relative
// repository path into the absolute directory restic will be pointed at.
//
// Nothing an administrator typed reaches this function. The mount point is
// derived from a registered disk identifier by the media adapter, and the
// relative path is a ROTUP constant. The checks here exist anyway, because this
// is the last point before a path becomes an argument to a tool that writes to
// removable media, and a containment proof is cheap compared with what it
// prevents.
func repositoryDirectory(mountPoint, relative string) (string, error) {
	if strings.TrimSpace(mountPoint) == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "repository_mount_missing",
			"a repository path requires a mount point")
	}
	if !filepath.IsAbs(mountPoint) {
		return "", domain.NewError(domain.ErrClassInvariant, "repository_mount_relative",
			"a repository mount point must be absolute")
	}
	normalized, err := domain.NormalizeRelativePath(relative, "repository path")
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "repository_path_empty",
			"a repository path must not be empty")
	}

	root := filepath.Clean(mountPoint)
	candidate := filepath.Clean(filepath.Join(root, filepath.FromSlash(normalized)))
	if !withinPath(root, candidate) || candidate == root {
		return "", domain.NewError(domain.ErrClassInvariant, "repository_path_escape",
			"the repository path escaped the disk's mount point")
	}
	if err := assertNoSymlinkEscape(root, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

// assertNoSymlinkEscape proves that a path, after symbolic links are resolved,
// is still below the mount point.
//
// A path that does not exist yet is checked through its nearest existing
// ancestor, because the interesting case is a link planted on the media that
// would redirect a repository ROTUP is about to create. Backup media is
// untrusted content: it can contain anything, including a symbolic link named
// "rotup" that points at the host filesystem.
func assertNoSymlinkEscape(root, candidate string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.NewError(domain.ErrClassUnavailable, "repository_mount_absent",
				"the disk's mount point does not exist, so the repository path cannot be resolved")
		}
		return domain.WrapError(domain.ErrClassInternal, "repository_path_unresolvable",
			"the disk's mount point could not be resolved", err)
	}

	current := candidate
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if !withinPath(resolvedRoot, resolved) {
				return domain.NewError(domain.ErrClassIntegrity, "repository_symlink_escape",
					"the repository path resolves outside the disk's mount point through a symbolic link")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return domain.WrapError(domain.ErrClassInternal, "repository_path_unresolvable",
				"the repository path could not be resolved", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return domain.NewError(domain.ErrClassInvariant, "repository_path_unresolvable",
				"the repository path could not be resolved to the disk's mount point")
		}
		current = parent
	}
}

func withinPath(root, candidate string) bool {
	if candidate == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(candidate, prefix)
}

// directoryState describes what is currently at the repository location.
type directoryState int

const (
	// directoryAbsent means nothing is there.
	directoryAbsent directoryState = iota
	// directoryEmpty means an empty directory is there.
	directoryEmpty
	// directoryPopulated means a directory with entries is there.
	directoryPopulated
	// directoryNotADirectory means something that is not a directory is there.
	directoryNotADirectory
)

// inspectDirectory reports what is at a path without following a symbolic link
// at the leaf. A link is deliberately not a directory here: ROTUP will not
// create a repository through one, and will not adopt whatever it points at.
func inspectDirectory(path string) (directoryState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return directoryAbsent, nil
		}
		return directoryAbsent, domain.WrapError(domain.ErrClassInternal, "repository_path_unreadable",
			"the repository location could not be inspected", err)
	}
	if !info.IsDir() {
		return directoryNotADirectory, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return directoryAbsent, domain.WrapError(domain.ErrClassInternal, "repository_path_unreadable",
			"the repository location could not be listed", err)
	}
	if len(entries) == 0 {
		return directoryEmpty, nil
	}
	return directoryPopulated, nil
}
