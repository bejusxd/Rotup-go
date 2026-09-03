package process

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/ports"
)

// The runner is exercised against the test binary itself, which keeps the
// tests portable: no external tool has to exist on the machine.
const helperMarker = "ROTUP_PROCESS_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperMarker) {
	case "":
		os.Exit(m.Run())
	case "exit-nonzero":
		os.Stdout.WriteString("some output\n")
		os.Stderr.WriteString("a failure explanation\n")
		os.Exit(7)
	case "flood":
		line := strings.Repeat("x", 1000) + "\n"
		for i := 0; i < 500; i++ {
			os.Stderr.WriteString(line)
		}
		os.Stderr.WriteString("FINAL-LINE\n")
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "dump-env":
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "ROTUP_") || strings.HasPrefix(entry, "SHOULD_NOT_") {
				os.Stdout.WriteString(entry + "\n")
			}
		}
		os.Exit(0)
	default:
		os.Stdout.WriteString("ok\n")
		os.Exit(0)
	}
}

func helperSpec(mode string, extra ...string) ports.CommandSpec {
	executable, _ := os.Executable()
	return ports.CommandSpec{
		Path: executable,
		Env:  append([]string{helperMarker + "=" + mode}, extra...),
	}
}

func TestRunCapturesSuccess(t *testing.T) {
	t.Parallel()

	result := NewRunner().Run(context.Background(), helperSpec("succeed"))
	if result.Err != nil {
		t.Fatalf("run: %v", result.Err)
	}
	if !result.Started || !result.Exited || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(string(result.Stdout), "ok") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if result.Duration <= 0 {
		t.Fatal("the runner must report how long the child ran")
	}
}

func TestNonZeroExitIsReportedNotErrored(t *testing.T) {
	t.Parallel()

	result := NewRunner().Run(context.Background(), helperSpec("exit-nonzero"))
	if result.Err != nil {
		t.Fatalf("a non-zero exit must not be an error: %v", result.Err)
	}
	if !result.Exited || result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(string(result.Stderr), "a failure explanation") {
		t.Fatalf("stderr = %q", result.Stderr)
	}
}

func TestMissingExecutableIsAnError(t *testing.T) {
	t.Parallel()

	result := NewRunner().Run(context.Background(), ports.CommandSpec{
		Path: "definitely-not-a-real-executable-xyz",
	})
	if result.Err == nil {
		t.Fatal("starting a missing executable must be an error")
	}
	if result.Started {
		t.Fatal("a process that never started must not be reported as started")
	}
}

func TestOutputIsBoundedToTheTail(t *testing.T) {
	t.Parallel()

	result := NewRunner().Run(context.Background(), ports.CommandSpec{
		Path:            helperSpec("flood").Path,
		Env:             helperSpec("flood").Env,
		StderrTailLimit: 4096,
	})
	if result.Err != nil {
		t.Fatalf("run: %v", result.Err)
	}
	if len(result.Stderr) > 4096 {
		t.Fatalf("stderr kept %d bytes, want at most 4096", len(result.Stderr))
	}
	if !result.StderrTruncated {
		t.Fatal("truncation must be reported")
	}
	// The tail is what matters: the final statistics line and the most recent
	// errors both appear at the end of an engine's output.
	if !strings.Contains(string(result.Stderr), "FINAL-LINE") {
		t.Fatal("the retained window must keep the end of the stream")
	}
}

func TestContextCancellationStopsTheChild(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	result := NewRunner().Run(ctx, helperSpec("sleep"))
	elapsed := time.Since(started)

	if elapsed > 10*time.Second {
		t.Fatalf("the child was not stopped: it ran for %v", elapsed)
	}
	if !result.TimedOut {
		t.Fatalf("an expired deadline must be reported as a timeout: %#v", result)
	}
	if result.Canceled {
		t.Fatal("a deadline must not be reported as a plain cancellation")
	}
}

func TestExplicitCancellationIsDistinguishedFromATimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	result := NewRunner().Run(ctx, helperSpec("sleep"))
	if !result.Canceled {
		t.Fatalf("explicit cancellation must be reported: %#v", result)
	}
	if result.TimedOut {
		t.Fatal("a cancellation must not be reported as a timeout")
	}
}

func TestEnvironmentIsReplacedNotInherited(t *testing.T) {
	t.Setenv("SHOULD_NOT_BE_INHERITED", "leaked")

	result := NewRunner().Run(context.Background(), helperSpec("dump-env"))
	if result.Err != nil {
		t.Fatalf("run: %v", result.Err)
	}
	if strings.Contains(string(result.Stdout), "SHOULD_NOT_BE_INHERITED") {
		t.Fatalf("the child inherited the parent environment: %q", result.Stdout)
	}
}

func TestTailBufferKeepsTheLastBytes(t *testing.T) {
	t.Parallel()

	buffer := newTailBuffer(10)
	if _, err := buffer.Write([]byte("abcde")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(buffer.Bytes()); got != "abcde" {
		t.Fatalf("buffer = %q", got)
	}
	if buffer.Truncated() {
		t.Fatal("a short write must not be reported as truncated")
	}

	if _, err := buffer.Write([]byte("fghijklmno")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(buffer.Bytes()); got != "fghijklmno" {
		t.Fatalf("buffer = %q, want the last ten bytes", got)
	}
	if !buffer.Truncated() {
		t.Fatal("an overflowing write must be reported as truncated")
	}

	// A single write larger than the whole window keeps only its tail.
	if _, err := buffer.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(buffer.Bytes()); got != "6789ABCDEF" {
		t.Fatalf("buffer = %q", got)
	}
}
