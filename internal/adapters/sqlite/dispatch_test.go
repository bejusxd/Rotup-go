package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestDispatchClaimIsExclusiveAndPreservesFIFO(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	scheduleFixture(t, s)
	for _, id := range []string{"first", "second"} {
		if _, err := s.ClaimScheduledRun(ctx, intentFor(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := s.DispatchQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].Run.ID != "first" {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimJobAttempt(ctx, queue[0], time.Now().UTC())
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("claims=%d", winners)
	}
	if queue, err := s.DispatchQueue(ctx); err != nil || len(queue) != 0 {
		t.Fatalf("later occurrence escaped active predecessor: %+v %v", queue, err)
	}
	current, err := s.DispatchIntent(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if current.AttemptCount != 1 || current.NominalDueAt != "2026-09-07T02:30" {
		t.Fatalf("metadata lost: %+v", current)
	}
	finished := current.Run
	finished.Status = domain.RunStatusSucceeded
	finished.FinishedAt = time.Now().UTC()
	if err := s.SetDispatchState(ctx, *current, finished, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDispatchState(ctx, queue[0], queue[0].Run, "stale", time.Time{}); err == nil {
		t.Fatal("stale writer overwrote completion")
	}
	queue, err = s.DispatchQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].Run.ID != "second" {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
}

func TestResumeOnlyUntouchedScheduledIntents(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	scheduleFixture(t, s)
	for _, id := range []string{"untouched", "attempted"} {
		if _, err := s.ClaimScheduledRun(ctx, intentFor(id, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status='INTERRUPTED',last_error_code='daemon_restarted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET attempt_count=1 WHERE id='attempted'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeUntouchedIntents(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"untouched", "attempted"} {
		run, err := s.Run(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := domain.RunStatusPending
		if id == "attempted" {
			want = domain.RunStatusInterrupted
		}
		if run.Status != want {
			t.Fatalf("%s=%s", id, run.Status)
		}
	}
}

func TestDispatchRetryTimesAreEnforcedInsideClaim(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	scheduleFixture(t, s)
	intent := intentFor("retry", "retry")
	if _, err := s.ClaimScheduledRun(ctx, intent); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Minute)
	deadline := now.Add(time.Hour)
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status='RETRY_DELAY',retry_not_before=?,retry_deadline_at=? WHERE id='retry'`, formatTime(next), formatTime(deadline)); err != nil {
		t.Fatal(err)
	}
	item, err := s.DispatchIntent(ctx, "retry")
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, deadline} {
		if ok, err := s.ClaimJobAttempt(ctx, *item, at); err != nil || ok {
			t.Fatalf("claim at %v = %v %v", at, ok, err)
		}
	}
	if ok, err := s.ClaimJobAttempt(ctx, *item, next); err != nil || !ok {
		t.Fatalf("due claim=%v %v", ok, err)
	}
}

func TestChainingUsesCommittedResultsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	job, _ := scheduleFixture(t, s)
	source := seedSource(t, s, "docs", "docs")
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET backup_after_collection=1 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimScheduledRun(ctx, intentFor("parent", "parent")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.ReconcileJobChains(ctx, now); err != nil {
		t.Fatal(err)
	}
	if runs, err := s.ListRuns(ctx, 100); err != nil || len(runs) != 1 {
		t.Fatal("uncommitted parent chained")
	}
	if err := s.SaveSourceResult(ctx, domain.SourceResult{RunID: "parent", SourceID: source.ID, Status: domain.RunStatusSucceeded, PublishedAt: now, StagingStateVersion: 1, StartedAt: now, FinishedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	parent, _ := s.Run(ctx, "parent")
	parent.Status = domain.RunStatusSucceeded
	parent.FinishedAt = now
	if err := s.UpdateRun(ctx, *parent); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.ReconcileJobChains(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListRuns(ctx, 100)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	for _, run := range runs {
		if run.ID == "parent" {
			continue
		}
		item, err := s.DispatchIntent(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := s.DispatchSourceIDs(ctx, *item)
		if err != nil || len(ids) != 1 || ids[0] != source.ID {
			t.Fatalf("child sources=%v %v", ids, err)
		}
		if item.ParentRunID != "parent" || run.Kind != domain.RunKindBackupLatest {
			t.Fatalf("child=%+v", item)
		}
	}
}
