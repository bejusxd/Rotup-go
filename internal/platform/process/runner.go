// Package process implements the ProcessRunner port with os/exec. It is kept
// portable so that M2 development and tests work on Windows; Linux process
// group and systemd hardening is a later milestone and belongs in the Linux
// platform adapter.
package process

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/ports"
)

// DefaultTailLimit is how many trailing bytes of each stream are retained when
// a caller does not choose its own bound.
const DefaultTailLimit = 64 * 1024

// Runner runs external tools from an executable path plus an argument array.
type Runner struct{}

// NewRunner returns the portable process runner.
func NewRunner() *Runner { return &Runner{} }

// Run executes spec and returns its bounded result. Run never returns an error
// for a non-zero exit code; that is reported through ProcessResult.ExitCode.
func (r *Runner) Run(ctx context.Context, spec ports.CommandSpec) ports.ProcessResult {
	stdout := newTailBuffer(limitOrDefault(spec.StdoutTailLimit))
	stderr := newTailBuffer(limitOrDefault(spec.StderrTailLimit))

	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	command.Dir = spec.Dir
	// A nil Env would make os/exec inherit the parent environment. The port
	// contract is that Env fully replaces it, so an explicit empty slice is
	// used to keep inherited variables out of the child.
	if spec.Env == nil {
		command.Env = []string{}
	} else {
		command.Env = spec.Env
	}
	command.Stdout = stdout
	command.Stderr = stderr

	started := time.Now()
	err := command.Run()
	duration := time.Since(started)

	result := ports.ProcessResult{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		Duration:        duration,
	}

	switch {
	case err == nil:
		result.Started = true
		result.Exited = true
		result.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.Started = true
			result.Exited = true
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Err = err
		}
	}

	// Context cancellation is reported separately because an interrupted
	// external tool must never be classified as an ordinary failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			result.TimedOut = true
		} else {
			result.Canceled = true
		}
	}
	return result
}

func limitOrDefault(limit int) int {
	if limit <= 0 {
		return DefaultTailLimit
	}
	return limit
}

// tailBuffer retains only the last limit bytes written to it. The tail is what
// matters for external tools: the final statistics line and the most recent
// errors both appear at the end of the stream.
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit, data: make([]byte, 0, min(limit, 8*1024))}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written := len(p)
	if written >= b.limit {
		b.data = append(b.data[:0], p[written-b.limit:]...)
		b.truncated = true
		return written, nil
	}
	b.data = append(b.data, p...)
	if overflow := len(b.data) - b.limit; overflow > 0 {
		b.data = append(b.data[:0], b.data[overflow:]...)
		b.truncated = true
	}
	return written, nil
}

func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}

func (b *tailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
