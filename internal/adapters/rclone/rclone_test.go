package rclone

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// recordingRunner captures the command specifications the adapter builds and
// replays a scripted result. It never starts a process, so the tests assert on
// exactly the argument array rclone would have received.
type recordingRunner struct {
	mu       sync.Mutex
	calls    []ports.CommandSpec
	configs  []string
	result   ports.ProcessResult
	onInvoke func(spec ports.CommandSpec)
	// respond builds the result from the invocation, which lets a test react to
	// the transient configuration the adapter just generated.
	respond func(spec ports.CommandSpec, config string) ports.ProcessResult
}

func (r *recordingRunner) Run(_ context.Context, spec ports.CommandSpec) ports.ProcessResult {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	// The transient configuration only exists while the child would be running,
	// so it has to be read here to be asserted on later.
	config := ""
	if path := configPathFrom(spec.Args); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			config = string(raw)
			r.configs = append(r.configs, config)
		}
	}
	respond := r.respond
	r.mu.Unlock()
	if r.onInvoke != nil {
		r.onInvoke(spec)
	}
	if respond != nil {
		return respond(spec, config)
	}
	return r.result
}

func (r *recordingRunner) lastCall(t *testing.T) ports.CommandSpec {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("the adapter never invoked the runner")
	}
	return r.calls[len(r.calls)-1]
}

func configPathFrom(args []string) string {
	for i, arg := range args {
		if arg == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// stubSecrets returns fixed material for the single locator it knows.
type stubSecrets struct{ password string }

func (s stubSecrets) Provider() string { return "stub" }

func (s stubSecrets) Put(context.Context, domain.SecretPurpose, string) (string, error) {
	return "locator", nil
}

func (s stubSecrets) Materialize(_ context.Context, locator string, fn func(domain.SecretMaterial) error) error {
	if locator != "locator" {
		return domain.NewError(domain.ErrClassConfiguration, "secret_missing", "no such secret")
	}
	return fn(domain.NewSecretMaterial(s.password))
}

func (s stubSecrets) Delete(context.Context, string) error { return nil }

// tempRuntimes hands out throwaway directories in place of the secret store's
// private runtime area.
type tempRuntimes struct{ base string }

func (r tempRuntimes) NewRuntimeDir() (string, func(), error) {
	dir, err := os.MkdirTemp(r.base, "runtime")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func newTestEngine(t *testing.T, runner *recordingRunner, password string) *Engine {
	t.Helper()
	return New(
		Config{ExecutablePath: filepath.Join(t.TempDir(), "rclone")},
		runner,
		stubSecrets{password: password},
		tempRuntimes{base: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func testSpec() ports.SourceSpec {
	return ports.SourceSpec{
		Host:         "fileserver.example",
		Share:        "docs",
		Subdirectory: "reports/2026",
		Username:     "reader",
	}
}

// newline avoids escaping troubles in the raw-string engine fixtures below.
var newline = string([]byte{10})

func argIndex(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

func TestCollectBuildsAPinnedArchivingArgumentArray(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, "hunter2")

	_, err := engine.Collect(context.Background(), ports.TransferRequest{
		Source:         testSpec(),
		Credential:     ports.CredentialHandle{Locator: "locator"},
		Mode:           ports.TransferSyncWithArchive,
		DestinationDir: filepath.Join(t.TempDir(), "current"),
		ArchiveDir:     filepath.Join(t.TempDir(), "revisions"),
		TempDir:        filepath.Join(t.TempDir(), "work"),
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	spec := runner.lastCall(t)
	if spec.Args[0] != "sync" {
		t.Fatalf("first argument = %q, want sync", spec.Args[0])
	}
	// The safety-affecting flags must be present and must come from code.
	for _, flag := range []string{"--backup-dir", "--delete-after", "--config", "--use-json-log", "--stats-log-level", "--temp-dir", "--cache-dir"} {
		if argIndex(spec.Args, flag) < 0 {
			t.Fatalf("argument array is missing %s: %v", flag, spec.Args)
		}
	}
	// The remote and the destination are the final two positional arguments.
	if got := spec.Args[len(spec.Args)-2]; got != "rotup_source:docs/reports/2026" {
		t.Fatalf("remote argument = %q", got)
	}
	if !strings.HasSuffix(spec.Args[len(spec.Args)-1], "current") {
		t.Fatalf("destination argument = %q", spec.Args[len(spec.Args)-1])
	}
}

func TestCopyModeNeverAsksTheEngineToDelete(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, "hunter2")

	_, err := engine.Collect(context.Background(), ports.TransferRequest{
		Source:         testSpec(),
		Mode:           ports.TransferCopy,
		DestinationDir: filepath.Join(t.TempDir(), "current"),
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	spec := runner.lastCall(t)
	if spec.Args[0] != "copy" {
		t.Fatalf("first argument = %q, want copy", spec.Args[0])
	}
	for _, forbidden := range []string{"sync", "--backup-dir", "--delete-after", "--delete-during", "--delete-before"} {
		if argIndex(spec.Args, forbidden) >= 0 {
			t.Fatalf("a copy-only transfer must not pass %s: %v", forbidden, spec.Args)
		}
	}
}

func TestTransferRequestsAreValidatedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, "hunter2")
	destination := filepath.Join(t.TempDir(), "current")

	cases := map[string]ports.TransferRequest{
		"archiving without an archive directory": {
			Source: testSpec(), Mode: ports.TransferSyncWithArchive, DestinationDir: destination,
		},
		"copy with an archive directory": {
			Source: testSpec(), Mode: ports.TransferCopy, DestinationDir: destination, ArchiveDir: "/tmp/x",
		},
		"unknown mode": {
			Source: testSpec(), Mode: ports.TransferMode("WIPE"), DestinationDir: destination,
		},
		"missing destination": {
			Source: testSpec(), Mode: ports.TransferCopy,
		},
		"flag-like destination": {
			Source: testSpec(), Mode: ports.TransferCopy, DestinationDir: "--config",
		},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := engine.Collect(context.Background(), request); err == nil {
				t.Fatal("the request should have been refused")
			}
		})
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 0 {
		t.Fatalf("a rejected request must never reach the runner, got %d calls", len(runner.calls))
	}
}

func TestPasswordNeverAppearsInArgumentsOrEnvironment(t *testing.T) {
	t.Parallel()

	const password = "s3cr3t-passphrase"
	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, password)

	if _, err := engine.TestConnection(context.Background(), testSpec(),
		ports.CredentialHandle{Locator: "locator"}); err != nil {
		t.Fatalf("test connection: %v", err)
	}

	spec := runner.lastCall(t)
	for _, arg := range spec.Args {
		if strings.Contains(arg, password) {
			t.Fatalf("the password reached an argument: %q", arg)
		}
	}
	for _, entry := range spec.Env {
		if strings.Contains(entry, password) {
			t.Fatalf("the password reached the child environment: %q", entry)
		}
	}
	if spec.Path == "" {
		t.Fatal("the adapter must invoke an explicit executable path")
	}

	// It reaches the child only through the transient configuration file, and
	// even there only in rclone's obscured form.
	runner.mu.Lock()
	configs := append([]string(nil), runner.configs...)
	runner.mu.Unlock()
	if len(configs) != 1 {
		t.Fatalf("expected exactly one transient configuration, got %d", len(configs))
	}
	if strings.Contains(configs[0], password) {
		t.Fatal("the transient configuration must not contain the plaintext password")
	}
	if !strings.Contains(configs[0], "pass = ") {
		t.Fatalf("the transient configuration has no password field: %q", configs[0])
	}
}

func TestTransientConfigurationIsRemovedAfterTheOperation(t *testing.T) {
	t.Parallel()

	var seenPath string
	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	runner.onInvoke = func(spec ports.CommandSpec) {
		seenPath = configPathFrom(spec.Args)
		if _, err := os.Stat(seenPath); err != nil {
			t.Errorf("the configuration must exist while the child runs: %v", err)
		}
	}
	engine := newTestEngine(t, runner, "hunter2")

	if _, err := engine.TestConnection(context.Background(), testSpec(),
		ports.CredentialHandle{Locator: "locator"}); err != nil {
		t.Fatalf("test connection: %v", err)
	}
	if seenPath == "" {
		t.Fatal("no configuration path was captured")
	}
	if _, err := os.Stat(seenPath); !os.IsNotExist(err) {
		t.Fatalf("the transient configuration must be removed after the child exits (err=%v)", err)
	}
}

func TestSourceWithoutCredentialWritesNoPasswordField(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, "unused")

	if _, err := engine.TestConnection(context.Background(), testSpec(), ports.CredentialHandle{}); err != nil {
		t.Fatalf("test connection: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.configs) != 1 {
		t.Fatalf("expected one configuration, got %d", len(runner.configs))
	}
	if strings.Contains(runner.configs[0], "pass = ") {
		t.Fatalf("a source without a stored credential must not get a password field: %q", runner.configs[0])
	}
}

func TestFilterRulesBecomeSeparateArguments(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := newTestEngine(t, runner, "hunter2")

	spec := testSpec()
	spec.ExcludeRules = []string{"*.tmp", "cache/**"}
	spec.IncludeRules = []string{"*.pdf"}

	if _, err := engine.Measure(context.Background(), spec, ports.CredentialHandle{}); err != nil {
		// Measure fails to parse the empty stdout; only the arguments matter here.
		var typed *domain.Error
		if !domain.AsError(err, &typed) || typed.Code != "rclone_size_unparsed" {
			t.Fatalf("measure: %v", err)
		}
	}

	args := runner.lastCall(t).Args
	excludeAt := argIndex(args, "--exclude")
	includeAt := argIndex(args, "--include")
	if excludeAt < 0 || includeAt < 0 {
		t.Fatalf("filter arguments are missing: %v", args)
	}
	if excludeAt > includeAt {
		t.Fatalf("exclude rules must be emitted before include rules: %v", args)
	}
	if args[excludeAt+1] != "*.tmp" {
		t.Fatalf("exclude rule = %q", args[excludeAt+1])
	}
	if args[includeAt+1] != "*.pdf" {
		t.Fatalf("include rule = %q", args[includeAt+1])
	}
}

func TestMeasureRefusesToGuessWhenOutputIsUnreadable(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{
		Started: true, Exited: true, Stdout: []byte("not json at all"),
	}}
	engine := newTestEngine(t, runner, "hunter2")

	size, err := engine.Measure(context.Background(), testSpec(), ports.CredentialHandle{})
	if err == nil {
		t.Fatal("an unreadable size must be an error, never an assumed zero")
	}
	if size.Count != 0 || size.Bytes != 0 {
		t.Fatalf("a failed measurement must return nothing, got %#v", size)
	}
}

func TestMeasureParsesMachineReadableOutput(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{
		Started: true, Exited: true, Stdout: []byte(`{"count":12,"bytes":3456}`),
	}}
	engine := newTestEngine(t, runner, "hunter2")

	size, err := engine.Measure(context.Background(), testSpec(), ports.CredentialHandle{})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if size.Count != 12 || size.Bytes != 3456 {
		t.Fatalf("size = %#v", size)
	}
	if argIndex(runner.lastCall(t).Args, "--json") < 0 {
		t.Fatal("the size probe must request machine-readable output")
	}
}

func TestTestConnectionCountsListedEntries(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{
		Started: true, Exited: true,
		Stdout: []byte(`[{"Path":"a.txt","IsDir":false},{"Path":"reports","IsDir":true}]`),
	}}
	engine := newTestEngine(t, runner, "hunter2")

	result, err := engine.TestConnection(context.Background(), testSpec(), ports.CredentialHandle{})
	if err != nil {
		t.Fatalf("test connection: %v", err)
	}
	if result.Entries != 2 {
		t.Fatalf("entries = %d, want 2", result.Entries)
	}
	if runner.lastCall(t).Args[0] != "lsjson" {
		t.Fatal("the connection test must use a read-only listing operation")
	}
}

func TestProbeParsesTheVersion(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{
		Started: true, Exited: true,
		Stdout: []byte("rclone v1.66.0\n- os/version: linux 6.1\n"),
	}}
	engine := newTestEngine(t, runner, "hunter2")

	info, err := engine.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Version != "1.66.0" || info.Name != "rclone" {
		t.Fatalf("engine info = %#v", info)
	}
}

func TestParseVersionHandlesRealAndUnexpectedOutput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"rclone v1.66.0":      "1.66.0",
		"rclone v1.70":        "1.70",
		"rclone 1.65.2 extra": "1.65.2",
		"":                    "",
		"something else":      "",
	}
	for input, want := range cases {
		if got := parseVersion(input); got != want {
			t.Fatalf("parseVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUnconfiguredEngineFailsClearly(t *testing.T) {
	t.Parallel()

	engine := New(Config{}, &recordingRunner{}, stubSecrets{}, tempRuntimes{base: t.TempDir()}, nil)
	if engine.Available() {
		t.Fatal("an engine with no executable path must not report as available")
	}
	if _, err := engine.Probe(context.Background()); err == nil {
		t.Fatal("probing an unconfigured engine must fail")
	}
	if _, err := engine.TestConnection(context.Background(), testSpec(), ports.CredentialHandle{}); err == nil {
		t.Fatal("using an unconfigured engine must fail")
	}
}

func TestClassifyMapsExitStatusAndOutput(t *testing.T) {
	t.Parallel()

	exited := func(code int) ports.ProcessResult {
		return ports.ProcessResult{Started: true, Exited: true, ExitCode: code}
	}
	cases := []struct {
		name   string
		result ports.ProcessResult
		text   string
		class  domain.ErrorClass
		code   string
	}{
		{"success", exited(0), "", "", ""},
		{"authentication", exited(2), "SMB: NT_STATUS_LOGON_FAILURE", domain.ErrClassAuthentication, "rclone_authentication_failed"},
		{"permission denied", exited(7), "read error: permission denied", domain.ErrClassAuthentication, "rclone_authentication_failed"},
		{"unreachable", exited(2), "dial tcp 10.0.0.5:445: connection refused", domain.ErrClassUnavailable, "rclone_unavailable"},
		{"missing share", exited(3), "", domain.ErrClassConfiguration, "rclone_path_not_found"},
		{"missing path text", exited(2), "NT_STATUS_OBJECT_PATH_NOT_FOUND", domain.ErrClassConfiguration, "rclone_path_not_found"},
		{"full disk", exited(2), "write error: no space left on device", domain.ErrClassCapacity, "rclone_no_space"},
		{"temporary", exited(5), "", domain.ErrClassUnavailable, "rclone_temporary_error"},
		{"usage", exited(1), "", domain.ErrClassInvariant, "rclone_usage_error"},
		{"timeout", ports.ProcessResult{TimedOut: true}, "", domain.ErrClassTimeout, "rclone_timeout"},
		{"canceled", ports.ProcessResult{Canceled: true}, "", domain.ErrClassCancellation, "rclone_canceled"},
		{"could not start", ports.ProcessResult{Err: errors.New("no such file")}, "", domain.ErrClassExternalTool, "rclone_start_failed"},
		{"no exit status", ports.ProcessResult{Started: true}, "", domain.ErrClassExternalTool, "rclone_no_exit"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := classify("the collection", testCase.result, testCase.text)
			if testCase.class == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a typed error")
			}
			if err.Class != testCase.class || err.Code != testCase.code {
				t.Fatalf("error = %s/%s, want %s/%s", err.Class, err.Code, testCase.class, testCase.code)
			}
			if strings.Contains(err.SafeMessage(), "NT_STATUS") {
				t.Fatalf("the safe message must not echo raw engine text: %q", err.SafeMessage())
			}
		})
	}
}

func TestCollectRedactsSecretsFromDiagnostics(t *testing.T) {
	t.Parallel()

	const password = "s3cr3t-passphrase"
	runner := &recordingRunner{}
	// The engine echoes back exactly what it was given: the obscured value from
	// the transient configuration and, in the worst case, the plaintext too.
	runner.respond = func(_ ports.CommandSpec, config string) ports.ProcessResult {
		obscured := passwordFieldFrom(config)
		stderr := strings.Join([]string{
			`{"level":"error","msg":"failed with pass = ` + obscured + `","object":"a.txt"}`,
			`{"level":"error","msg":"and the plaintext ` + password + ` leaked too"}`,
			`{"level":"notice","msg":"done","stats":{"bytes":10,"transfers":1,"errors":2,"deletes":0}}`,
		}, newline)
		return ports.ProcessResult{Started: true, Exited: true, ExitCode: 2, Stderr: []byte(stderr)}
	}
	engine := newTestEngine(t, runner, password)

	result, collectErr := engine.Collect(context.Background(), ports.TransferRequest{
		Source:         testSpec(),
		Credential:     ports.CredentialHandle{Locator: "locator"},
		Mode:           ports.TransferCopy,
		DestinationDir: filepath.Join(t.TempDir(), "current"),
	})
	if collectErr == nil {
		t.Fatal("a non-zero exit must produce an error")
	}
	if strings.Contains(result.Diagnostics, password) {
		t.Fatalf("diagnostics leaked the plaintext password: %q", result.Diagnostics)
	}
	runner.mu.Lock()
	obscured := passwordFieldFrom(runner.configs[0])
	runner.mu.Unlock()
	if obscured == "" {
		t.Fatal("the transient configuration had no password field")
	}
	if strings.Contains(result.Diagnostics, obscured) {
		t.Fatalf("diagnostics leaked the obscured password: %q", result.Diagnostics)
	}
	if strings.Count(result.Diagnostics, "[REDACTED]") != 2 {
		t.Fatalf("diagnostics should redact both forms: %q", result.Diagnostics)
	}
	if !result.Stats.Available || result.Stats.Errors != 2 || result.Stats.Transfers != 1 {
		t.Fatalf("statistics = %#v", result.Stats)
	}
}

// passwordFieldFrom extracts the obscured value the adapter wrote.
func passwordFieldFrom(config string) string {
	for _, line := range strings.Split(config, newline) {
		if value, ok := strings.CutPrefix(line, "pass = "); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func TestCollectReportsTruncatedOutput(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{
		Started: true, Exited: true, StderrTruncated: true,
		Stderr: []byte(`{"level":"notice","msg":"done","stats":{"bytes":1,"transfers":1}}`),
	}}
	engine := newTestEngine(t, runner, "hunter2")

	result, err := engine.Collect(context.Background(), ports.TransferRequest{
		Source:         testSpec(),
		Mode:           ports.TransferCopy,
		DestinationDir: filepath.Join(t.TempDir(), "current"),
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "truncated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("truncated output must be reported as a warning, got %#v", result.Warnings)
	}
}

func TestParseLogOutputKeepsTheFinalStatisticsRecord(t *testing.T) {
	t.Parallel()

	raw := strings.Join([]string{
		`ansfers":1}}`, // a fragment, because the stream is a retained tail
		`{"level":"notice","msg":"interim","stats":{"bytes":10,"transfers":1}}`,
		`not json`,
		`{"level":"warning","msg":"a file was skipped","object":"b.txt"}`,
		`{"level":"notice","msg":"final","stats":{"bytes":99,"transfers":4,"checks":7,"deletes":2,"deletedDirs":1,"errors":0,"totalBytes":120}}`,
	}, "\n")

	parsed := parseLogOutput(raw)
	if !parsed.Stats.Available {
		t.Fatal("statistics should have been parsed")
	}
	if parsed.Stats.Bytes != 99 || parsed.Stats.Transfers != 4 || parsed.Stats.Checks != 7 {
		t.Fatalf("statistics = %#v", parsed.Stats)
	}
	if parsed.Stats.Deletes != 3 {
		t.Fatalf("deletes = %d, want file and directory removals combined", parsed.Stats.Deletes)
	}
	if !parsed.Stats.MutationObserved() {
		t.Fatal("transfers and deletes must count as an observed mutation")
	}
	if len(parsed.NoticeLines) != 1 || !strings.Contains(parsed.NoticeLines[0], "b.txt") {
		t.Fatalf("warning lines = %#v", parsed.NoticeLines)
	}
}

func TestParseLogOutputReportsNoStatisticsWhenThereAreNone(t *testing.T) {
	t.Parallel()

	parsed := parseLogOutput("plain text output with no json at all")
	if parsed.Stats.Available {
		t.Fatal("absent statistics must not be reported as available")
	}
	if parsed.Stats.MutationObserved() {
		t.Fatal("unavailable statistics must not claim anything about mutation")
	}
}

func TestDiagnosticsAreBounded(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	for i := 0; i < maxDiagnosticLines*3; i++ {
		builder.WriteString(`{"level":"error","msg":"` + strings.Repeat("x", maxDiagnosticLineLength*2) + `"}` + "\n")
	}
	parsed := parseLogOutput(builder.String())

	if len(parsed.ErrorLines) != maxDiagnosticLines {
		t.Fatalf("retained %d error lines, want the bound of %d", len(parsed.ErrorLines), maxDiagnosticLines)
	}
	for _, line := range parsed.ErrorLines {
		if len(line) > maxDiagnosticLineLength+4 {
			t.Fatalf("a diagnostic line of %d bytes exceeded the bound", len(line))
		}
	}
}

func TestObscureRoundTripsAndIsNotPlaintext(t *testing.T) {
	t.Parallel()

	for _, password := range []string{"hunter2", "unicode pässwörd ✓", strings.Repeat("long", 100)} {
		obscured, err := obscure(password)
		if err != nil {
			t.Fatalf("obscure(%q): %v", password, err)
		}
		if obscured == password || strings.Contains(obscured, password) {
			t.Fatalf("obscured value %q still contains the plaintext", obscured)
		}
		revealed, err := reveal(obscured)
		if err != nil {
			t.Fatalf("reveal: %v", err)
		}
		if revealed != password {
			t.Fatalf("round trip produced %q, want %q", revealed, password)
		}
	}

	// A random initialisation vector means the same password never produces the
	// same stored form twice.
	first, _ := obscure("hunter2")
	second, _ := obscure("hunter2")
	if first == second {
		t.Fatal("obscured values must not be deterministic")
	}
}

func TestEngineHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{result: ports.ProcessResult{Canceled: true}}
	engine := newTestEngine(t, runner, "hunter2")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.Collect(ctx, ports.TransferRequest{
		Source:         testSpec(),
		Mode:           ports.TransferCopy,
		DestinationDir: filepath.Join(t.TempDir(), "current"),
	})
	if err == nil {
		t.Fatal("a canceled transfer must return an error")
	}
	var typed *domain.Error
	if !domain.AsError(err, &typed) || typed.Class != domain.ErrClassCancellation {
		t.Fatalf("error = %v, want a cancellation class", err)
	}
}

func TestEngineAppliesAConfiguredTimeout(t *testing.T) {
	t.Parallel()

	var deadline time.Time
	runner := &recordingRunner{result: ports.ProcessResult{Started: true, Exited: true}}
	engine := New(
		Config{ExecutablePath: filepath.Join(t.TempDir(), "rclone"), ConnectionTimeout: 250 * time.Millisecond},
		&deadlineRunner{inner: runner, deadline: &deadline},
		stubSecrets{},
		tempRuntimes{base: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if _, err := engine.TestConnection(context.Background(), testSpec(), ports.CredentialHandle{}); err != nil {
		t.Fatalf("test connection: %v", err)
	}
	if deadline.IsZero() {
		t.Fatal("the adapter must impose a deadline on the child process")
	}
	if remaining := time.Until(deadline); remaining > time.Second {
		t.Fatalf("deadline is %v away, want the configured 250ms bound", remaining)
	}
}

// deadlineRunner records the deadline the adapter attached to the context.
type deadlineRunner struct {
	inner    *recordingRunner
	deadline *time.Time
}

func (r *deadlineRunner) Run(ctx context.Context, spec ports.CommandSpec) ports.ProcessResult {
	if value, ok := ctx.Deadline(); ok {
		*r.deadline = value
	}
	return r.inner.Run(ctx, spec)
}

func TestMinimalEnvironmentCarriesNothingUnexpected(t *testing.T) {
	t.Setenv("ROTUP_SHOULD_NOT_LEAK", "leaked-value")
	for _, entry := range minimalEnvironment() {
		if strings.Contains(entry, "ROTUP_SHOULD_NOT_LEAK") || strings.Contains(entry, "leaked-value") {
			t.Fatalf("the child environment inherited %q", entry)
		}
	}
}
