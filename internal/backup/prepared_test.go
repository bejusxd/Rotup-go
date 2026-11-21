package backup

import (
	"context"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestPreparedBackupKeepsExistingRunAndDurabilityBoundary(t *testing.T) {
	h := newHarness(t)
	repo := h.establishRepository()
	source := h.seedSource("docs", 1)
	ctx := context.Background()
	id, _ := domain.NewID()
	run := domain.Run{ID: id, Kind: domain.RunKindBackupLatest, TriggerKind: domain.RunTriggerScheduled, Status: domain.RunStatusRunning, RequestedAt: h.now, UpdatedAt: h.now}
	if err := h.store.RecordRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	work, release, err := h.service.PrepareJob(ctx, run, repo.ID, []string{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !h.barrier.BeingRead(source.ID) {
		t.Fatal("barrier not reserved")
	}
	if err := work(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	got, err := h.store.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Status.Successful() {
		t.Fatalf("backup=%s %s", got.Status, got.SafeError)
	}
	metadata, err := h.store.BackupRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DurableSuccessAt.IsZero() || metadata.UnmountVerifiedAt.IsZero() {
		t.Fatalf("missing durability: %+v", metadata)
	}
	if h.barrier.BeingRead(source.ID) {
		t.Fatal("barrier leaked")
	}
}
