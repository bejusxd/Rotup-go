package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

// The crash boundary.
//
// Every other test in this package recovers a backup inside the process that
// created it: the run row always reaches a terminal state through some code
// path, even when that path is a failure. That leaves the one case startup
// recovery actually exists for completely unproven — a daemon that dies outright
// while a backup is in flight, leaving a RUNNING row nobody will ever finish and
// a real snapshot on the media that nobody confirmed.
//
// This spike closes that gap with a real process boundary. A worker process runs
// the genuine orchestration against real restic and real SQLite, is frozen at the
// exact persistence boundary the crash matters at, and is then killed outright
// with SIGKILL. A fresh service opens the same database afterwards and has to
// work out what happened from what is durably recorded, with no help from the
// process that wrote it.
//
// What is real here: restic, the repository, the snapshot, SQLite, the
// orchestration, the process death. What is not: the media. The lease is the
// package's fake, so this proves nothing about mount, flush or unmount on real
// hardware, and it is not USB acceptance.
//
// Opt-in, and confined to a directory the test owns:
//
//	ROTUP_RESTIC_SPIKE=1 ROTUP_RESTIC_PATH=/path/to/restic \
//	  go test ./internal/backup/ -run SpikeCrash -v
const (
	// crashWorkerEnv carries the shared root to the worker process. Its presence
	// is also what turns the worker test on, so an ordinary run skips it.
	crashWorkerEnv = "ROTUP_SPIKE_CRASH_ROOT"
	// crashReadyFile is written once the worker is parked at the boundary, with
	// the snapshot durably recorded and the run still RUNNING.
	crashReadyFile = "worker-parked"
)

// TestSpikeCrashWorker is the child process. It is not a test of anything on its
// own: it exists to be killed. Without the environment variable it does nothing,
// so a normal `go test` run skips it.
func TestSpikeCrashWorker(t *testing.T) {
	root := os.Getenv(crashWorkerEnv)
	if root == "" {
		t.Skip("worker process for TestSpikeCrashedBackupIsSweptThenAdopted")
	}

	h := newSpikeHarnessAt(t, root)
	repository := h.establishRepository()
	source := h.seedSource("documents", 2)
	h.seedRevisionBatch(source, true)

	// Park inside SyncAndUnmount. By the time the service calls it, restic has
	// really written the snapshot, RecordCandidateSnapshot has committed it, and
	// the snapshot has been independently verified and recorded — but nothing has
	// been marked synced, unmounted or complete. That is precisely the window
	// where a dead process leaves evidence that is good enough to adopt and a run
	// row that claims nothing.
	h.media.onUnmount = func(context.Context) {
		if err := os.WriteFile(filepath.Join(root, crashReadyFile), []byte("parked"), 0o600); err != nil {
			t.Errorf("signal the parent: %v", err)
			return
		}
		// Sleeping rather than blocking on a bare channel: a permanently parked
		// goroutine with no timer can trip the runtime's deadlock detector, and
		// this process must die from the parent's signal, not from a panic.
		for {
			time.Sleep(time.Second)
		}
	}

	if _, err := h.service.StartBackup(context.Background(), repository.ID, []string{source.ID}); err != nil {
		t.Fatalf("worker backup: %v", err)
	}
	t.Fatal("the worker was expected to be killed at the durability boundary, not to finish")
}

// A daemon killed mid-backup leaves a RUNNING row and a real snapshot. Startup
// recovery must sweep the row without granting anything, and only an explicit
// recovery may adopt it — once.
func TestSpikeCrashedBackupIsSweptThenAdopted(t *testing.T) {
	resticPath := requireResticSpike(t)
	root := t.TempDir()
	ctx := context.Background()

	// --- the crash ----------------------------------------------------------
	worker := exec.Command(os.Args[0], "-test.run=^TestSpikeCrashWorker$")
	worker.Env = append(os.Environ(), crashWorkerEnv+"="+root)
	worker.Stdout, worker.Stderr = os.Stderr, os.Stderr
	if err := worker.Start(); err != nil {
		t.Fatalf("start the worker process: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- worker.Wait() }()
	killed := false
	defer func() {
		// Only ever this process, and only if it somehow outlived the test.
		if !killed && worker.Process != nil {
			_ = worker.Process.Kill()
			<-exited
		}
	}()

	parked := filepath.Join(root, crashReadyFile)
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	giveUp := time.After(3 * time.Minute)
	for ready := false; !ready; {
		select {
		case err := <-exited:
			killed = true
			t.Fatalf("the worker exited before reaching the durability boundary: %v", err)
		case <-giveUp:
			t.Fatal("the worker never reached the durability boundary")
		case <-poll.C:
			if _, err := os.Stat(parked); err == nil {
				ready = true
			}
		}
	}

	if err := worker.Process.Kill(); err != nil { // SIGKILL: no cleanup, no unwinding.
		t.Fatalf("kill the worker: %v", err)
	}
	killed = true
	if err := <-exited; err == nil {
		t.Fatal("the worker exited normally; it was supposed to be killed")
	}
	t.Log("worker killed at the durability boundary")

	// --- what the dead process left behind ----------------------------------
	h := newSpikeHarnessAt(t, root)
	disk, err := h.store.ListDisks(ctx)
	if err != nil || len(disk) != 1 {
		t.Fatalf("load the disk the worker registered: %v / %d", err, len(disk))
	}
	h.disk = disk[0]
	repository, err := h.store.RepositoryForDisk(ctx, h.disk.ID)
	if err != nil || repository == nil {
		t.Fatalf("load the repository the worker established: %v", err)
	}
	h.repository = *repository

	sources, err := h.store.ListSources(ctx)
	if err != nil || len(sources) != 1 {
		t.Fatalf("load the source the worker seeded: %v / %d", err, len(sources))
	}
	batches, err := h.store.ListRevisionBatches(ctx, sources[0].ID, 10)
	if err != nil || len(batches) != 1 {
		t.Fatalf("load the revision batch: %v / %d", err, len(batches))
	}
	batch := batches[0]

	crashed := h.backupRunOfKind(ctx, domain.RunKindBackupLatest)
	if crashed.CandidateSnapshotID == "" {
		t.Fatal("the crash left no durable candidate, so the boundary was wrong")
	}
	if crashed.Stage == domain.StageDurableSuccess {
		t.Fatal("a killed process left a durable success behind")
	}
	before := h.run(crashed.RunID)
	if before.Status != domain.RunStatusRunning {
		t.Fatalf("the crashed run is %s, want RUNNING; the crash boundary was wrong", before.Status)
	}
	if len(h.coverage(batch.ID)) != 0 {
		t.Fatal("a killed process granted revision coverage")
	}
	t.Logf("crashed run %s left candidate snapshot %s at stage %s",
		crashed.RunID, crashed.CandidateSnapshotID, crashed.Stage)

	// The snapshot really is on the media, written by the process that died.
	inspector, spec := h.spikeInspector(t)
	snapshots, err := inspector.Snapshots(ctx, spec)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("the crashed backup's snapshot is not readable: %v / %d", err, len(snapshots))
	}

	// --- startup recovery ---------------------------------------------------
	report, err := h.service.Recover(ctx)
	if err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	if report.InterruptedRuns != 1 {
		t.Fatalf("startup recovery swept %d runs, want 1", report.InterruptedRuns)
	}
	swept := h.run(crashed.RunID)
	if swept.Status != domain.RunStatusInterrupted {
		t.Fatalf("swept run status = %s, want INTERRUPTED", swept.Status)
	}
	if swept.Status.Successful() || len(h.coverage(batch.ID)) != 0 {
		t.Fatal("startup recovery granted success or coverage on its own")
	}
	if health := h.repositoryNow().Health; health != domain.RepositorySuspect {
		t.Fatalf("repository health after the sweep = %s, want SUSPECT", health)
	}

	// --- explicit recovery adopts, exactly once -----------------------------
	recoveryID, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID)
	if err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	if run := h.run(recoveryID); run.Status != domain.RunStatusSucceeded {
		t.Fatalf("recovery did not succeed: %s / %s", run.Status, run.SafeError)
	}
	adopted := h.run(crashed.RunID)
	if !adopted.Status.Successful() {
		t.Fatalf("the crashed run was not adopted: %s / %s", adopted.Status, adopted.SafeError)
	}
	adoptedBackup := h.backupRun(crashed.RunID)
	if adoptedBackup.Durability != domain.DurabilityAdopted {
		t.Fatalf("durability = %s, want ADOPTED_AFTER_INTERRUPTION", adoptedBackup.Durability)
	}
	if adoptedBackup.VerifiedSnapshotID != crashed.CandidateSnapshotID {
		t.Fatalf("adopted a different snapshot than the crash recorded: %s vs %s",
			adoptedBackup.VerifiedSnapshotID, crashed.CandidateSnapshotID)
	}
	coverage := h.coverage(batch.ID)
	if len(coverage) != 1 || coverage[0].SnapshotID != adoptedBackup.VerifiedSnapshotID {
		t.Fatalf("coverage after adoption = %#v", coverage)
	}
	if health := h.repositoryNow().Health; health != domain.RepositoryHealthy {
		t.Fatalf("repository health after adoption = %s", health)
	}

	// Running recovery again must not adopt a second time or duplicate coverage.
	if _, err := h.service.StartRepositoryRecovery(ctx, h.repository.ID); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	if again := h.coverage(batch.ID); len(again) != 1 {
		t.Fatalf("a repeated recovery changed coverage: %#v", again)
	}

	// --- the bytes ----------------------------------------------------------
	// Restoring with the restic binary directly, rather than asking ROTUP to
	// confirm its own adoption.
	after, err := inspector.Snapshots(ctx, spec)
	if err != nil || len(after) != 1 || after[0].ID != adoptedBackup.VerifiedSnapshotID {
		t.Fatalf("recovery did not preserve exactly the adopted snapshot: %v / %#v", err, after)
	}
	target := filepath.Join(t.TempDir(), "restored")
	passwordFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passwordFile, []byte(testPassword), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	out, err := exec.CommandContext(ctx, resticPath, "-r", spec.Directory,
		"--password-file", passwordFile, "--no-cache", "--no-lock",
		"restore", adoptedBackup.VerifiedSnapshotID, "--target", target).CombinedOutput()
	if err != nil {
		t.Fatalf("restore the adopted snapshot: %v\n%s", err, out)
	}
	assertRestoredFile(t, target, filepath.Join(h.layout.Root(), "sources", "documents", "current", "document.txt"), "hello")
	assertRestoredFile(t, target, filepath.Join(h.layout.Root(),
		filepath.FromSlash(batch.RelativePath), "displaced.txt"), "old")
}

// assertRestoredFile checks one file inside a restic restore target. restic
// recreates absolute source paths under the target, so the original path is
// rejoined beneath it.
func assertRestoredFile(t *testing.T, target, originalPath, want string) {
	t.Helper()
	restored := filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(
		filepath.ToSlash(originalPath), string(filepath.Separator))))
	if volume := filepath.VolumeName(originalPath); volume != "" {
		t.Skipf("restore path mapping is not modelled for volume-qualified paths (%s)", volume)
	}
	content, err := os.ReadFile(restored)
	if err != nil {
		t.Fatalf("read restored %s: %v", restored, err)
	}
	if string(content) != want {
		t.Fatalf("restored %s = %q, want %q", restored, content, want)
	}
}

// backupRunOfKind finds the single backup run of a kind, without assuming the
// test knows an identifier the crashed process generated.
func (h *harness) backupRunOfKind(ctx context.Context, kind domain.RunKind) domain.BackupRun {
	h.t.Helper()
	runs, err := h.store.ListRuns(ctx, 50)
	if err != nil {
		h.t.Fatalf("list runs: %v", err)
	}
	var found *domain.BackupRun
	for _, run := range runs {
		if run.Kind != kind {
			continue
		}
		backup, err := h.store.BackupRun(ctx, run.ID)
		if err != nil {
			h.t.Fatalf("load backup run: %v", err)
		}
		if backup == nil {
			continue
		}
		if found != nil {
			h.t.Fatal("more than one backup run was recorded")
		}
		found = backup
	}
	if found == nil {
		h.t.Fatalf("no %s run was recorded", kind)
	}
	return *found
}
