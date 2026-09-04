package system

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FileState is what inspection needs to know about a path. It is a value type
// so every rule that decides whether a file is installed correctly is a pure
// function over it.
type FileState struct {
	Exists    bool
	IsDir     bool
	IsSymlink bool
	Mode      fs.FileMode
	UID       int
	GID       int
	Size      int64
}

// ErrNotSupported is returned by Host operations that this platform cannot
// perform. Inspection turns it into a plain finding rather than a crash, which
// is what lets doctor run on a developer's non-Linux workstation.
var ErrNotSupported = errors.New("not supported on this platform")

// Host is the entire surface this package uses to observe and change a machine.
// Keeping it this small is what makes the provisioning logic testable without
// root: a recording implementation is a few dozen lines.
type Host interface {
	// GOOS names the platform.
	GOOS() string
	// Euid is the effective user id of this process, or -1 where the concept
	// does not apply.
	Euid() int
	// CurrentUser is the account this process runs as.
	CurrentUser() string
	// LookupEnv reads a process environment variable.
	LookupEnv(key string) (string, bool)
	// LookupAccount resolves an account name to its uid, primary group name
	// and gid.
	LookupAccount(name string) (uid int, group string, gid int, err error)
	// LookPath finds an executable on PATH.
	LookPath(file string) (string, error)

	// Lstat observes a path without following a final symlink. A path that does
	// not exist is not an error; it is a FileState with Exists false.
	Lstat(path string) (FileState, error)
	// ReadFile reads a whole file.
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes content to a temporary file beside path and
	// renames it into place, so a reader never sees a half-written policy file.
	WriteFileAtomic(path string, content []byte, mode fs.FileMode) error
	// CopyExecutableAtomic installs an executable, replacing any existing one
	// by rename.
	CopyExecutableAtomic(source, destination string, mode fs.FileMode) error
	// MkdirAll creates a directory tree.
	MkdirAll(path string, mode fs.FileMode) error
	// Chmod sets a mode.
	Chmod(path string, mode fs.FileMode) error
	// Chown sets ownership.
	Chown(path string, uid, gid int) error

	// Run executes a command and returns its combined output. It is used for
	// version probes, visudo validation and systemctl.
	Run(ctx context.Context, name string, args ...string) (output string, exitCode int, err error)
}

// OSHost is the real implementation.
//
// Root prefixes every absolute path this package touches. In production it is
// empty. Tests set it to a temporary directory so the same provisioning code
// runs against real files with real modes, without touching the machine.
type OSHost struct {
	Root string
	// Env overrides the process environment when non-nil, so a test can
	// describe an environment without mutating the running process.
	Env map[string]string
	// Accounts overrides account resolution when non-nil.
	Accounts map[string]struct {
		UID   int
		Group string
		GID   int
	}
	// Path overrides executable discovery when non-nil. A name mapped to an
	// empty string is treated as absent.
	Path map[string]string
	// Commands maps an executable path to a canned result, so version probes
	// and visudo can be exercised without those tools being installed.
	Commands map[string]CommandResult
	// SkipChown makes Chown a no-op. Provisioning still records that it would
	// have changed ownership, which is what the idempotence tests assert on.
	SkipChown bool
}

// CommandResult is a canned Run outcome for tests.
type CommandResult struct {
	Output   string
	ExitCode int
	Err      error
}

// NewOSHost returns a host bound to the real machine.
func NewOSHost() *OSHost { return &OSHost{} }

func (h *OSHost) resolve(path string) string {
	if h.Root == "" {
		return path
	}
	return filepath.Join(h.Root, path)
}

// GOOS reports the platform.
func (h *OSHost) GOOS() string { return runtime.GOOS }

// Euid reports the effective user id.
func (h *OSHost) Euid() int { return os.Geteuid() }

// CurrentUser reports this process's account name, falling back to the numeric
// id when the name service cannot answer.
func (h *OSHost) CurrentUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return strconv.Itoa(os.Geteuid())
}

// LookupEnv reads an environment variable.
func (h *OSHost) LookupEnv(key string) (string, bool) {
	if h.Env != nil {
		value, ok := h.Env[key]
		return value, ok
	}
	return os.LookupEnv(key)
}

// LookupAccount resolves an account to its numeric identity and primary group.
func (h *OSHost) LookupAccount(name string) (int, string, int, error) {
	if h.Accounts != nil {
		account, ok := h.Accounts[name]
		if !ok {
			return 0, "", 0, errors.New("no such account: " + name)
		}
		return account.UID, account.Group, account.GID, nil
	}
	found, err := user.Lookup(name)
	if err != nil {
		return 0, "", 0, err
	}
	uid, err := strconv.Atoi(found.Uid)
	if err != nil {
		return 0, "", 0, err
	}
	gid, err := strconv.Atoi(found.Gid)
	if err != nil {
		return 0, "", 0, err
	}
	groupName := found.Gid
	if group, err := user.LookupGroupId(found.Gid); err == nil && group.Name != "" {
		groupName = group.Name
	}
	return uid, groupName, gid, nil
}

// LookPath finds an executable.
func (h *OSHost) LookPath(file string) (string, error) {
	if h.Path != nil {
		found, ok := h.Path[file]
		if !ok || found == "" {
			return "", exec.ErrNotFound
		}
		return found, nil
	}
	return exec.LookPath(file)
}

// Lstat observes a path. A missing path returns a zero FileState and no error.
func (h *OSHost) Lstat(path string) (FileState, error) {
	info, err := os.Lstat(h.resolve(path))
	if errors.Is(err, fs.ErrNotExist) {
		return FileState{}, nil
	}
	if err != nil {
		return FileState{}, err
	}
	state := FileState{
		Exists:    true,
		IsDir:     info.IsDir(),
		IsSymlink: info.Mode()&fs.ModeSymlink != 0,
		Mode:      info.Mode(),
		Size:      info.Size(),
		UID:       -1,
		GID:       -1,
	}
	if uid, gid, ok := ownerOf(info); ok {
		state.UID, state.GID = uid, gid
	}
	return state, nil
}

// ReadFile reads a file.
func (h *OSHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(h.resolve(path)) }

// WriteFileAtomic writes through a temporary file in the destination directory
// and renames it, so a concurrent reader sees either the old file or the new
// one and never a truncated policy.
func (h *OSHost) WriteFileAtomic(path string, content []byte, mode fs.FileMode) error {
	resolved := h.resolve(path)
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(resolved), ".rotup-setup-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Set the mode before the rename: the file must never be visible at its
	// final path with the temporary file's default permissions.
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, resolved)
}

// CopyExecutableAtomic installs an executable by the same replace-by-rename
// rule, so a running process keeps its open image and the next invocation gets
// the new one.
func (h *OSHost) CopyExecutableAtomic(source, destination string, mode fs.FileMode) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return h.WriteFileAtomic(destination, content, mode)
}

// MkdirAll creates a directory tree.
func (h *OSHost) MkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(h.resolve(path), mode)
}

// Chmod sets a mode.
func (h *OSHost) Chmod(path string, mode fs.FileMode) error {
	return os.Chmod(h.resolve(path), mode)
}

// Chown sets ownership.
func (h *OSHost) Chown(path string, uid, gid int) error {
	if h.SkipChown {
		return nil
	}
	return chownPath(h.resolve(path), uid, gid)
}

// Run executes a command with a bounded deadline and returns its combined
// output. The environment is inherited: these are diagnostic probes and
// package tools, never the privileged helper, which the daemon invokes through
// its own hardened path.
func (h *OSHost) Run(ctx context.Context, name string, args ...string) (string, int, error) {
	if h.Commands != nil {
		result, ok := h.Commands[name]
		if !ok {
			return "", -1, exec.ErrNotFound
		}
		return result.Output, result.ExitCode, result.Err
	}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	command := exec.CommandContext(runCtx, name, args...)
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitCode(), nil
	}
	if err != nil {
		return text, -1, err
	}
	return text, 0, nil
}
