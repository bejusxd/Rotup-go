// Package restic adapts the restic executable to the BackupEngine port.
//
// It follows the same subprocess rules the collection engine already follows:
// everything restic is told is an executable path plus an argument array, no
// command string is ever assembled, no shell is involved, safety-affecting
// flags are pinned here rather than accepted from configuration, and the
// repository password reaches restic only through a transient, permission
// restricted file in a private runtime directory that is deleted as soon as the
// child exits.
//
// The adapter deliberately exposes no destructive operation. There is no
// forget, no prune, and no unlock: those belong to the retention milestone, and
// code that cannot express them cannot be persuaded to perform one.
package restic

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
)

const (
	passwordFileName = "restic.pass"

	// defaultOutputTailLimit bounds the retained tail of most streams.
	defaultOutputTailLimit = 256 * 1024
	// backupTailLimit is larger because restic streams progress messages and
	// the terminal summary must survive at the end of the retained tail.
	backupTailLimit = 4 * 1024 * 1024
	// listingTailLimit bounds a snapshot listing, which is parsed as a whole.
	listingTailLimit = 8 * 1024 * 1024
)

// Config selects the executable and the per-operation time limits.
type Config struct {
	// ExecutablePath is the explicit path to the restic binary.
	ExecutablePath string
	ProbeTimeout   time.Duration
	InspectTimeout time.Duration
	BackupTimeout  time.Duration
	CheckTimeout   time.Duration
}

func (c Config) withDefaults() Config {
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 30 * time.Second
	}
	if c.InspectTimeout <= 0 {
		c.InspectTimeout = 10 * time.Minute
	}
	if c.BackupTimeout <= 0 {
		c.BackupTimeout = 6 * time.Hour
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 2 * time.Hour
	}
	return c
}

// RuntimeDirProvider supplies a private volatile directory for transient engine
// material. It is satisfied by the secret store, which owns that policy.
type RuntimeDirProvider interface {
	NewRuntimeDir() (string, func(), error)
}

// Engine implements ports.BackupEngine using the restic executable.
type Engine struct {
	cfg      Config
	runner   ports.ProcessRunner
	secrets  ports.SecretStore
	runtimes RuntimeDirProvider
	logger   *slog.Logger
}

// New builds the adapter. A nil logger falls back to the default logger.
func New(cfg Config, runner ports.ProcessRunner, secretStore ports.SecretStore,
	runtimes RuntimeDirProvider, logger *slog.Logger) *Engine {

	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		cfg:      cfg.withDefaults(),
		runner:   runner,
		secrets:  secretStore,
		runtimes: runtimes,
		logger:   logger.With("component", "restic"),
	}
}

// Available reports whether an executable path is configured at all.
func (e *Engine) Available() bool { return strings.TrimSpace(e.cfg.ExecutablePath) != "" }

// Probe runs `restic version` and returns the discovered engine.
func (e *Engine) Probe(ctx context.Context) (ports.EngineInfo, error) {
	if !e.Available() {
		return ports.EngineInfo{}, errEngineUnconfigured()
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.ProbeTimeout)
	defer cancel()

	result := e.runner.Run(ctx, ports.CommandSpec{
		Path:            e.cfg.ExecutablePath,
		Args:            []string{"version"},
		Env:             minimalEnvironment(),
		StdoutTailLimit: 16 * 1024,
		StderrTailLimit: 16 * 1024,
	})
	text := string(result.Stdout) + "\n" + string(result.Stderr)
	if err := classify("the restic version probe", result, text); err != nil {
		return ports.EngineInfo{}, err
	}
	version := parseVersion(text)
	if version == "" {
		return ports.EngineInfo{}, domain.NewError(domain.ErrClassExternalTool, "restic_version_unparsed",
			"the restic version could not be determined from its output")
	}
	return ports.EngineInfo{Name: "restic", Version: version, Path: e.cfg.ExecutablePath}, nil
}

// InitializeRepository creates a new restic repository and reads its identity
// back from the repository itself.
//
// The read-back is not a formality. `restic init` reporting success says a
// repository was written; only opening it afterwards and reading its
// configuration proves the repository ROTUP will later trust actually exists
// and is the one that was just created.
func (e *Engine) InitializeRepository(ctx context.Context, spec ports.RepositorySpec) (ports.RepositoryInfo, error) {
	var info ports.RepositoryInfo
	err := e.withSession(ctx, spec, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.InspectTimeout)
		defer cancel()

		args := append([]string{"init"}, session.commonArgs()...)
		result := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: defaultOutputTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})
		text := session.redact(string(result.Stdout) + "\n" + string(result.Stderr))
		if failure := classify("initializing the repository", result, text); failure != nil {
			return failure
		}

		read, err := e.readConfig(runCtx, session)
		if err != nil {
			return err
		}
		info = read
		return nil
	})
	return info, err
}

// InspectRepository opens an existing repository and reads its identity back.
func (e *Engine) InspectRepository(ctx context.Context, spec ports.RepositorySpec) (ports.RepositoryInfo, error) {
	var info ports.RepositoryInfo
	err := e.withSession(ctx, spec, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.InspectTimeout)
		defer cancel()

		read, err := e.readConfig(runCtx, session)
		if err != nil {
			return err
		}
		info = read
		return nil
	})
	return info, err
}

// readConfig runs `restic cat config`, which is restic's own statement of the
// repository's stable identity.
func (e *Engine) readConfig(ctx context.Context, session *session) (ports.RepositoryInfo, error) {
	args := append([]string{"cat", "config"}, session.commonArgs()...)
	result := e.runner.Run(ctx, ports.CommandSpec{
		Path:            e.cfg.ExecutablePath,
		Args:            args,
		Env:             minimalEnvironment(),
		StdoutTailLimit: defaultOutputTailLimit,
		StderrTailLimit: defaultOutputTailLimit,
	})
	text := session.redact(string(result.Stdout) + "\n" + string(result.Stderr))
	if failure := classify("reading the repository configuration", result, text); failure != nil {
		return ports.RepositoryInfo{}, failure
	}
	info, ok := parseRepositoryConfig(result.Stdout)
	if !ok {
		return ports.RepositoryInfo{}, domain.NewError(domain.ErrClassIntegrity,
			domain.RepositoryErrorUnreadable,
			"restic opened the repository but its configuration could not be read, so the repository identity is unproven")
	}
	normalized, err := domain.ValidateResticRepositoryID(info.ID)
	if err != nil {
		return ports.RepositoryInfo{}, err
	}
	info.ID = normalized
	return info, nil
}

// Backup creates one snapshot of exactly the requested paths.
//
// It returns a result even when the operation failed, because the caller has to
// decide what the repository state must become, and a failure that may have
// written to the repository is a different situation from one that cannot have.
func (e *Engine) Backup(ctx context.Context, request ports.BackupRequest) (ports.BackupResult, error) {
	if err := validateBackupRequest(request); err != nil {
		return ports.BackupResult{}, err
	}

	var outcome ports.BackupResult
	err := e.withSession(ctx, request.Repository, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.BackupTimeout)
		defer cancel()

		args := []string{"backup"}
		args = append(args, session.commonArgs()...)
		args = append(args, "--json")
		// restic must snapshot exactly what it is given: crossing into another
		// filesystem partway down a staging path would put bytes into the
		// snapshot that are not part of the captured read-set.
		//
		// restic only implements this on Unix, and refuses to start when it is
		// passed on Windows. The production target is Linux, so the guarantee
		// is kept where it is available rather than dropped everywhere; the
		// Windows development build runs without it and is not a supported
		// deployment. Asking the platform is the honest way to express that —
		// silently passing an unsupported flag would turn every backup into a
		// startup failure.
		if oneFileSystemSupported() {
			args = append(args, "--one-file-system")
		}
		for _, tag := range request.Tags {
			args = append(args, "--tag", tag)
		}
		// The path separator guards against a future path that begins with "-";
		// validateBackupRequest already refuses one, and this makes the
		// argument array unambiguous regardless.
		args = append(args, "--")
		args = append(args, request.Paths...)

		result := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: backupTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})

		stats := parseBackupOutput(result.Stdout)
		// restic writes source error records to stderr as well as progress and
		// summary records to stdout. Neither stream may hide a partial read.
		stats.Errors += parseBackupOutput(result.Stderr).Errors
		outcome.Stats = stats
		outcome.Duration = result.Duration
		outcome.Diagnostics = session.redact(diagnosticTail(result))
		if result.StdoutTruncated {
			outcome.Warnings = append(outcome.Warnings,
				"restic produced more output than ROTUP retains, so only the end of its report was kept")
		}
		if stats.Available && stats.Errors > 0 {
			outcome.Warnings = append(outcome.Warnings,
				"restic reported "+formatCount(stats.Errors)+" error(s) while reading the backup paths")
		}

		text := session.redact(string(result.Stdout) + "\n" + string(result.Stderr))
		if failure := classify("the backup", result, text); failure != nil {
			return failure
		}

		// A clean exit is not on its own proof. restic's own structured output
		// has to agree that it produced a snapshot, and name it.
		if signal := stats.FailureSignal(); signal != "" {
			return domain.NewError(domain.ErrClassExternalTool, "restic_reported_incomplete",
				"restic exited successfully but its own output says the backup did not complete: "+signal)
		}
		if _, err := domain.ValidateSnapshotID(stats.SnapshotID); err != nil {
			return err
		}
		return nil
	})
	return outcome, err
}

// Snapshots lists the repository's snapshots.
func (e *Engine) Snapshots(ctx context.Context, spec ports.RepositorySpec) ([]ports.Snapshot, error) {
	var snapshots []ports.Snapshot
	err := e.withSession(ctx, spec, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.InspectTimeout)
		defer cancel()

		args := append([]string{"snapshots"}, session.commonArgs()...)
		args = append(args, "--json")
		result := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: listingTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})
		text := session.redact(string(result.Stdout) + "\n" + string(result.Stderr))
		if failure := classify("listing the repository snapshots", result, text); failure != nil {
			return failure
		}
		if result.StdoutTruncated {
			// A truncated listing cannot be used to prove a snapshot is absent,
			// and could not be decoded anyway. Saying so is the only safe
			// answer.
			return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorUnreadable,
				"the repository snapshot listing was larger than ROTUP retains, so it could not be read completely")
		}
		listed, ok := parseSnapshots(result.Stdout)
		if !ok {
			return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorUnreadable,
				"the repository snapshot listing could not be read")
		}
		snapshots = listed
		return nil
	})
	return snapshots, err
}

// Check runs restic's non-destructive structural verification. It reads
// repository metadata and index structure; it never reads or rewrites pack
// data, and it never deletes anything.
func (e *Engine) Check(ctx context.Context, spec ports.RepositorySpec) (ports.CheckResult, error) {
	var outcome ports.CheckResult
	err := e.withSession(ctx, spec, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.CheckTimeout)
		defer cancel()

		args := append([]string{"check"}, session.commonArgs()...)
		args = append(args, "--json")
		result := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: defaultOutputTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})
		outcome.Duration = result.Duration
		outcome.Diagnostics = session.redact(diagnosticTail(result))
		outcome.Errors = countCheckErrors(result.Stdout) + countCheckErrors(result.Stderr)

		text := session.redact(string(result.Stdout) + "\n" + string(result.Stderr))
		if failure := classify("the repository check", result, text); failure != nil {
			if outcome.Errors == 0 {
				outcome.Errors = 1
			}
			return failure
		}
		if outcome.Errors > 0 {
			return domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorUnreadable,
				"the repository check reported "+formatCount(outcome.Errors)+" problem(s)")
		}
		return nil
	})
	return outcome, err
}

// validateBackupRequest refuses a request ROTUP itself built incorrectly. These
// are invariant failures rather than configuration errors: every path here was
// produced by the staging layout, so a violation is a defect.
func validateBackupRequest(request ports.BackupRequest) error {
	if len(request.Paths) == 0 {
		return domain.NewError(domain.ErrClassInvariant, "restic_paths_missing",
			"a backup requires at least one path to snapshot")
	}
	for _, path := range request.Paths {
		if !filepath.IsAbs(path) {
			return domain.NewError(domain.ErrClassInvariant, "restic_path_relative",
				"a backup path must be absolute")
		}
		if strings.HasPrefix(path, "-") {
			return domain.NewError(domain.ErrClassInvariant, "restic_path_flaglike",
				"a backup path would be read as a restic flag")
		}
	}
	for _, tag := range request.Tags {
		if err := validateTag(tag); err != nil {
			return err
		}
	}
	return nil
}

// validateTag keeps snapshot tags to safe identifiers. Tags are stored inside
// the repository and are shown in the UI, so nothing user-supplied and nothing
// that could be read as a flag belongs in one.
func validateTag(tag string) error {
	if tag == "" || len(tag) > 128 {
		return domain.NewError(domain.ErrClassInvariant, "restic_tag_invalid",
			"a snapshot tag has an unacceptable length")
	}
	if strings.HasPrefix(tag, "-") {
		return domain.NewError(domain.ErrClassInvariant, "restic_tag_invalid",
			"a snapshot tag must not start with \"-\"")
	}
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == ':' || r == '.':
		default:
			return domain.NewError(domain.ErrClassInvariant, "restic_tag_invalid",
				"a snapshot tag contains a character ROTUP does not put in repository metadata")
		}
	}
	return nil
}

// session holds the transient per-invocation password file plus the redactor
// for the material it contains.
type session struct {
	directory    string
	passwordPath string
	redactor     *secrets.Redactor
}

// commonArgs are the flags every restic invocation carries.
//
// --no-cache is deliberate. A restic cache would have to live on protected
// local storage, would need its own size policy, and would introduce a second
// place where repository content is remembered between operations. M4 does not
// need any of that, and an operation that reads only from the media it just
// verified is easier to reason about.
func (s *session) commonArgs() []string {
	return []string{
		"--repo", s.directory,
		"--password-file", s.passwordPath,
		"--no-cache",
	}
}

func (s *session) redact(text string) string { return s.redactor.Redact(text) }

// withSession materializes the repository password into a private runtime
// directory, invokes fn, and removes the directory afterwards.
//
// The password is never an argument and never an environment value: restic is
// given a path to read it from, and that path stops existing when the operation
// ends.
func (e *Engine) withSession(ctx context.Context, spec ports.RepositorySpec, fn func(*session) error) error {
	if !e.Available() {
		return errEngineUnconfigured()
	}
	if err := validateRepositoryDirectory(spec.Directory); err != nil {
		return err
	}
	if spec.Credential.Locator == "" {
		return domain.NewError(domain.ErrClassConfiguration, "repository_password_missing",
			"this repository has no stored password, so it cannot be opened")
	}

	runtimeDir, cleanup, err := e.runtimes.NewRuntimeDir()
	if err != nil {
		return err
	}
	defer cleanup()

	current := &session{directory: spec.Directory, redactor: secrets.NewRedactor()}
	return e.secrets.Materialize(ctx, spec.Credential.Locator, func(material domain.SecretMaterial) error {
		if material.Empty() {
			return domain.NewError(domain.ErrClassConfiguration, "repository_password_missing",
				"the stored repository password is empty, so the repository cannot be opened")
		}
		current.redactor.Add(material.Reveal())
		path, err := secrets.WriteRuntimeFile(runtimeDir, passwordFileName, []byte(material.Reveal()))
		if err != nil {
			return err
		}
		current.passwordPath = path
		return fn(current)
	})
}

// validateRepositoryDirectory refuses a repository location ROTUP could not
// have produced. The backup service builds this path from a verified mount
// point, so anything else is a defect rather than a configuration mistake.
func validateRepositoryDirectory(directory string) error {
	if strings.TrimSpace(directory) == "" {
		return domain.NewError(domain.ErrClassInvariant, "repository_directory_missing",
			"a repository operation requires a repository directory")
	}
	if !filepath.IsAbs(directory) {
		return domain.NewError(domain.ErrClassInvariant, "repository_directory_relative",
			"a repository directory must be absolute")
	}
	if strings.HasPrefix(directory, "-") {
		return domain.NewError(domain.ErrClassInvariant, "repository_directory_flaglike",
			"a repository directory would be read as a restic flag")
	}
	return nil
}

// diagnosticTail assembles the bounded engine output kept for history. It is
// redacted by the caller before it is stored anywhere.
func diagnosticTail(result ports.ProcessResult) string {
	var parts []string
	if len(result.Stderr) > 0 {
		parts = append(parts, strings.TrimSpace(string(result.Stderr)))
	}
	joined := strings.Join(parts, "\n")
	const limit = 8 * 1024
	if len(joined) > limit {
		joined = joined[len(joined)-limit:]
	}
	return joined
}

// oneFileSystemSupported reports whether restic implements --one-file-system on
// this platform. It does not on Windows, where passing the flag makes restic
// refuse to run at all.
func oneFileSystemSupported() bool { return runtime.GOOS != "windows" }

// minimalEnvironment returns the smallest environment the child needs. The
// inherited environment is otherwise discarded so nothing ROTUP holds, and no
// RESTIC_* variable an operator may have exported, reaches the tool. Windows
// needs SystemRoot for its own runtime; Linux needs nothing.
func minimalEnvironment() []string {
	if runtime.GOOS != "windows" {
		return []string{}
	}
	allowed := []string{"SystemRoot", "SystemDrive", "windir", "TEMP", "TMP"}
	env := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
