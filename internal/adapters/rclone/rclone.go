// Package rclone adapts the rclone executable to the TransferEngine port.
//
// Everything rclone is told is built as an executable path plus an argument
// array: no command string is ever assembled and no shell is involved. Safety
// affecting flags are pinned here rather than accepted from configuration, and
// the SMB password reaches rclone only through a transient, permission
// restricted configuration file in a private runtime directory that is deleted
// as soon as the child exits.
package rclone

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/secrets"
)

// remoteName is the fixed name of the transient remote. It never contains
// administrator input, so it can never be read as a flag or a second argument.
const remoteName = "rotup_source"

const (
	configFileName = "rclone.conf"

	// defaultOutputTailLimit bounds the retained tail of each engine stream.
	defaultOutputTailLimit = 256 * 1024
	// listingTailLimit is larger because a connection test parses stdout.
	listingTailLimit = 1024 * 1024
)

// Config selects the executable and the per-operation time limits.
type Config struct {
	// ExecutablePath is the explicit path to the rclone binary.
	ExecutablePath    string
	ProbeTimeout      time.Duration
	ConnectionTimeout time.Duration
	MeasureTimeout    time.Duration
	CollectTimeout    time.Duration
}

func (c Config) withDefaults() Config {
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 30 * time.Second
	}
	if c.ConnectionTimeout <= 0 {
		c.ConnectionTimeout = 60 * time.Second
	}
	if c.MeasureTimeout <= 0 {
		c.MeasureTimeout = 30 * time.Minute
	}
	if c.CollectTimeout <= 0 {
		c.CollectTimeout = 2 * time.Hour
	}
	return c
}

// RuntimeDirProvider supplies a private volatile directory for transient engine
// material. It is satisfied by the secret store, which owns that policy.
type RuntimeDirProvider interface {
	NewRuntimeDir() (string, func(), error)
}

// Engine implements ports.TransferEngine using the rclone executable.
type Engine struct {
	cfg      Config
	runner   ports.ProcessRunner
	secrets  ports.SecretStore
	runtimes RuntimeDirProvider
	logger   *slog.Logger
}

// New builds the adapter. A nil logger falls back to the default logger.
func New(cfg Config, runner ports.ProcessRunner, secretStore ports.SecretStore, runtimes RuntimeDirProvider, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		cfg:      cfg.withDefaults(),
		runner:   runner,
		secrets:  secretStore,
		runtimes: runtimes,
		logger:   logger.With("component", "rclone"),
	}
}

// Available reports whether an executable path is configured at all.
func (e *Engine) Available() bool { return strings.TrimSpace(e.cfg.ExecutablePath) != "" }

// Probe runs `rclone version` and returns the discovered engine.
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
	if err := classify("the rclone version probe", result, text); err != nil {
		return ports.EngineInfo{}, err
	}
	version := parseVersion(text)
	if version == "" {
		return ports.EngineInfo{}, domain.NewError(domain.ErrClassExternalTool, "rclone_version_unparsed",
			"the rclone version could not be determined from its output")
	}
	return ports.EngineInfo{Name: "rclone", Version: version, Path: e.cfg.ExecutablePath}, nil
}

// TestConnection lists the top level of the configured share subtree. It is
// read-only and never touches staging.
func (e *Engine) TestConnection(ctx context.Context, spec ports.SourceSpec, credential ports.CredentialHandle) (ports.ConnectionResult, error) {
	var result ports.ConnectionResult
	err := e.withSession(ctx, spec, credential, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.ConnectionTimeout)
		defer cancel()

		args := []string{"lsjson", "--max-depth", "1"}
		args = append(args, session.commonArgs()...)
		args = append(args, filterArgs(spec)...)
		args = append(args, session.remotePath())

		process := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: listingTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})
		stderr := session.redact(string(process.Stderr))
		if failure := classify("the connection test", process, stderr); failure != nil {
			return failure
		}

		result.Duration = process.Duration
		result.Entries = -1
		if !process.StdoutTruncated {
			var entries []listingEntry
			if err := json.Unmarshal(process.Stdout, &entries); err == nil {
				result.Entries = len(entries)
			}
		}
		return nil
	})
	return result, err
}

// Measure enumerates the source without writing anything. Its count is what the
// collector uses to detect a source that has unexpectedly become empty, so the
// same include and exclude rules the transfer would use are applied here.
func (e *Engine) Measure(ctx context.Context, spec ports.SourceSpec, credential ports.CredentialHandle) (ports.SourceSize, error) {
	var size ports.SourceSize
	err := e.withSession(ctx, spec, credential, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.MeasureTimeout)
		defer cancel()

		args := []string{"size", "--json"}
		args = append(args, session.commonArgs()...)
		args = append(args, filterArgs(spec)...)
		args = append(args, session.remotePath())

		process := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: 64 * 1024,
			StderrTailLimit: defaultOutputTailLimit,
		})
		stderr := session.redact(string(process.Stderr))
		if failure := classify("the source enumeration", process, stderr); failure != nil {
			return failure
		}

		var parsed sizeOutput
		if err := json.Unmarshal(trimToJSONObject(process.Stdout), &parsed); err != nil {
			// Refusing to guess is essential: a size ROTUP cannot read must not
			// be treated as zero, because zero is what blocks a destructive
			// reconciliation.
			return domain.WrapError(domain.ErrClassExternalTool, "rclone_size_unparsed",
				"the source enumeration produced output ROTUP could not read", err)
		}
		size = ports.SourceSize{Count: parsed.Count, Bytes: parsed.Bytes}
		return nil
	})
	return size, err
}

// Collect performs the transfer. The result is populated even when the returned
// error is non-nil so the collector can decide whether staging is still
// trustworthy.
func (e *Engine) Collect(ctx context.Context, request ports.TransferRequest) (ports.TransferResult, error) {
	if err := validateTransferRequest(request); err != nil {
		return ports.TransferResult{}, err
	}
	var result ports.TransferResult
	err := e.withSession(ctx, request.Source, request.Credential, func(session *session) error {
		runCtx, cancel := context.WithTimeout(ctx, e.cfg.CollectTimeout)
		defer cancel()

		args := collectArgs(request, session)
		process := e.runner.Run(runCtx, ports.CommandSpec{
			Path:            e.cfg.ExecutablePath,
			Args:            args,
			Env:             minimalEnvironment(),
			StdoutTailLimit: defaultOutputTailLimit,
			StderrTailLimit: defaultOutputTailLimit,
		})

		stderr := session.redact(string(process.Stderr))
		parsed := parseLogOutput(stderr)
		result.Stats = parsed.Stats
		result.Duration = process.Duration
		result.Diagnostics = joinDiagnostics(parsed.ErrorLines)
		result.Warnings = parsed.NoticeLines
		if process.StderrTruncated {
			result.Warnings = append(result.Warnings,
				"engine output was truncated; only the most recent messages were kept")
		}
		// classify returns a typed pointer, so it is compared before being
		// returned: a nil *domain.Error assigned straight to an error would
		// become a non-nil interface and turn a clean run into a failure.
		if failure := classify("the collection", process, stderr); failure != nil {
			return failure
		}
		// A zero exit status is necessary but not sufficient. rclone also
		// reports its own fatal-error, retry-exhausted, error-count and
		// queued-versus-completed counters, and any of those contradicting a
		// clean exit means the destination is not a complete transfer. ROTUP
		// refuses the run rather than trusting the exit code alone.
		if parsed.Stats.Available {
			if signal := parsed.Stats.FailureSignal(); signal != "" {
				return domain.NewError(domain.ErrClassExternalTool, "rclone_reported_incomplete",
					"rclone exited successfully but its own counters say the collection did not complete: "+signal)
			}
		}
		return nil
	})
	return result, err
}

// collectArgs builds the pinned argument array for one transfer. Safety
// affecting flags live here, in code, and are never administrator supplied.
func collectArgs(request ports.TransferRequest, session *session) []string {
	var args []string
	switch request.Mode {
	case ports.TransferCopy:
		// copy adds and updates; it never removes anything from the
		// destination, which is exactly the copy_only policy.
		args = []string{"copy"}
	case ports.TransferSyncWithArchive:
		// sync makes the destination match the source. --backup-dir turns every
		// overwrite and every deletion into a move into the run's revision
		// batch, so nothing is destroyed. --delete-after keeps rclone from
		// removing anything until all transfers are done, which means a failed
		// transfer phase cannot be followed by a deletion phase.
		args = []string{"sync", "--backup-dir", request.ArchiveDir, "--delete-after"}
	}
	args = append(args, session.commonArgs()...)
	if request.TempDir != "" {
		args = append(args, "--temp-dir", request.TempDir)
	}
	args = append(args, filterArgs(request.Source)...)
	args = append(args, session.remotePath(), request.DestinationDir)
	return args
}

// filterArgs renders include and exclude rules. Excludes are emitted first and
// therefore win: rclone applies filter rules in the order they appear, so an
// excluded path stays excluded even if a later include would match it. Using
// any include also makes rclone exclude everything it does not match.
func filterArgs(spec ports.SourceSpec) []string {
	args := make([]string, 0, 2*(len(spec.ExcludeRules)+len(spec.IncludeRules)))
	for _, rule := range spec.ExcludeRules {
		args = append(args, "--exclude", rule)
	}
	for _, rule := range spec.IncludeRules {
		args = append(args, "--include", rule)
	}
	return args
}

func validateTransferRequest(request ports.TransferRequest) error {
	switch request.Mode {
	case ports.TransferCopy:
		if request.ArchiveDir != "" {
			return domain.NewError(domain.ErrClassInvariant, "rclone_archive_unexpected",
				"a copy-only transfer must not be given an archive directory")
		}
	case ports.TransferSyncWithArchive:
		if request.ArchiveDir == "" {
			return domain.NewError(domain.ErrClassInvariant, "rclone_archive_missing",
				"an archiving transfer requires an archive directory")
		}
	default:
		return domain.NewError(domain.ErrClassInvariant, "rclone_mode_unknown",
			fmt.Sprintf("unknown transfer mode %q", string(request.Mode)))
	}
	if request.DestinationDir == "" {
		return domain.NewError(domain.ErrClassInvariant, "rclone_destination_missing",
			"a transfer requires a destination directory")
	}
	for _, path := range []string{request.DestinationDir, request.ArchiveDir, request.TempDir} {
		if strings.HasPrefix(path, "-") {
			return domain.NewError(domain.ErrClassInvariant, "rclone_path_flaglike",
				"a filesystem path would be read as an rclone flag")
		}
	}
	return nil
}

// session holds the transient per-invocation configuration file plus the
// redactor for the material it contains.
type session struct {
	spec       ports.SourceSpec
	configPath string
	cacheDir   string
	redactor   *secrets.Redactor
}

func (s *session) commonArgs() []string {
	return []string{
		"--config", s.configPath,
		"--cache-dir", s.cacheDir,
		"--use-json-log",
		"--log-level", "NOTICE",
		"--stats-log-level", "NOTICE",
		"--stats", "1m",
	}
}

// remotePath renders the single remote argument. The remote name is a fixed
// literal and the share and subdirectory were validated by the domain, so this
// value can never begin with "-" or contain a separator that escapes the share.
func (s *session) remotePath() string {
	path := remoteName + ":" + s.spec.Share
	if s.spec.Subdirectory != "" {
		path += "/" + s.spec.Subdirectory
	}
	return path
}

func (s *session) redact(text string) string { return s.redactor.Redact(text) }

// withSession materializes the credential, writes the transient configuration
// into a private runtime directory, invokes fn, and removes the directory.
func (e *Engine) withSession(ctx context.Context, spec ports.SourceSpec, credential ports.CredentialHandle, fn func(*session) error) error {
	if !e.Available() {
		return errEngineUnconfigured()
	}
	runtimeDir, cleanup, err := e.runtimes.NewRuntimeDir()
	if err != nil {
		return err
	}
	defer cleanup()

	current := &session{
		spec:     spec,
		cacheDir: runtimeDir,
		redactor: secrets.NewRedactor(),
	}

	build := func(material domain.SecretMaterial) error {
		contents, obscured, err := renderConfig(spec, material)
		if err != nil {
			return err
		}
		// Both the plaintext and rclone's reversible obscured form are
		// redacted, because an engine error message may echo either.
		if !material.Empty() {
			current.redactor.Add(material.Reveal())
			current.redactor.Add(obscured)
		}
		path, err := secrets.WriteRuntimeFile(runtimeDir, configFileName, []byte(contents))
		if err != nil {
			return err
		}
		current.configPath = path
		return fn(current)
	}

	if credential.Locator == "" {
		return build(domain.SecretMaterial{})
	}
	return e.secrets.Materialize(ctx, credential.Locator, build)
}

// renderConfig builds the transient rclone configuration. It returns the file
// contents and the obscured password so both forms can be redacted from output.
func renderConfig(spec ports.SourceSpec, material domain.SecretMaterial) (string, string, error) {
	var builder strings.Builder
	builder.WriteString("[" + remoteName + "]\n")
	builder.WriteString("type = smb\n")
	builder.WriteString("host = " + spec.Host + "\n")
	if spec.Username != "" {
		builder.WriteString("user = " + spec.Username + "\n")
	}
	obscured := ""
	if !material.Empty() {
		var err error
		obscured, err = obscure(material.Reveal())
		if err != nil {
			return "", "", err
		}
		builder.WriteString("pass = " + obscured + "\n")
	}
	return builder.String(), obscured, nil
}

// minimalEnvironment returns the smallest environment the child needs. The
// inherited environment is otherwise discarded so nothing ROTUP holds leaks into
// an external tool. Windows networking APIs need SystemRoot, so it is passed
// through there; Linux needs nothing.
func minimalEnvironment() []string {
	if runtime.GOOS != "windows" {
		return []string{}
	}
	allowed := []string{"SystemRoot", "SystemDrive", "windir"}
	env := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func errEngineUnconfigured() error {
	return domain.NewError(domain.ErrClassConfiguration, "rclone_unconfigured",
		"no rclone executable is configured; set ROTUP_RCLONE_PATH")
}

func joinDiagnostics(lines []string) string {
	return strings.Join(lines, "\n")
}

// trimToJSONObject discards anything before the first "{" so a stray banner
// line cannot break decoding of a single JSON object.
func trimToJSONObject(raw []byte) []byte {
	index := strings.IndexByte(string(raw), '{')
	if index <= 0 {
		return raw
	}
	return raw[index:]
}
