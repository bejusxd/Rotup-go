package collector

import (
	"context"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestPreparedCollectionUsesExistingIntentAndReleasesResources(t *testing.T) {
	h := newHarness(t, domain.PolicyArchiveThenSync)
	h.writeSource("a.txt", "A")
	ctx := context.Background()
	id, _ := domain.NewID()
	run := domain.Run{ID: id, Kind: domain.RunKindCollection, TriggerKind: domain.RunTriggerScheduled, Status: domain.RunStatusRunning, RequestedAt: time.Now(), UpdatedAt: time.Now()}
	if err := h.store.RecordRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	work, release, err := h.service.PrepareJob(ctx, run, []string{h.source.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !h.service.Busy(h.source.ID) {
		t.Fatal("source not reserved")
	}
	if err := work(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	got, source := h.reload(id)
	if !got.Status.Successful() || source.StagingStateVersion != 1 {
		t.Fatalf("result=%+v source=%+v", got, source)
	}
	runs, err := h.store.ListRuns(ctx, 100)
	if err != nil || len(runs) != 1 {
		t.Fatalf("extra run: %v %v", runs, err)
	}
	if h.service.Busy(source.ID) {
		t.Fatal("resource leak")
	}
}
