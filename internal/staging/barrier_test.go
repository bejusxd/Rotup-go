package staging

import (
	"sync"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

func TestBarrierBlocksCollectionWhileBackupReads(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	releaseRead, err := barrier.AcquireRead([]string{"source-a"})
	if err != nil {
		t.Fatalf("acquire read: %v", err)
	}

	if _, err := barrier.AcquireMutation("source-a"); err == nil {
		t.Fatal("collection was allowed to mutate a source a backup is reading")
	} else if code := errorCode(t, err); code != "staging_read_in_progress" {
		t.Fatalf("mutation error code = %q, want staging_read_in_progress", code)
	}

	releaseRead()
	release, err := barrier.AcquireMutation("source-a")
	if err != nil {
		t.Fatalf("collection was still blocked after the backup released the barrier: %v", err)
	}
	release()
}

func TestBarrierBlocksBackupWhileCollectionMutates(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	releaseMutation, err := barrier.AcquireMutation("source-a")
	if err != nil {
		t.Fatalf("acquire mutation: %v", err)
	}

	if _, err := barrier.AcquireRead([]string{"source-a"}); err == nil {
		t.Fatal("a backup was allowed to read a source a collection is reconciling")
	} else if code := errorCode(t, err); code != "staging_mutation_in_progress" {
		t.Fatalf("read error code = %q, want staging_mutation_in_progress", code)
	}

	releaseMutation()
	release, err := barrier.AcquireRead([]string{"source-a"})
	if err != nil {
		t.Fatalf("the backup was still blocked after the collection finished: %v", err)
	}
	release()
}

func TestBarrierAlsoBlocksUnselectedSources(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	releaseRead, err := barrier.AcquireRead([]string{"source-a"})
	if err != nil {
		t.Fatalf("acquire read: %v", err)
	}
	defer releaseRead()

	if _, err := barrier.AcquireMutation("source-b"); err == nil {
		t.Fatal("the global barrier allowed mutation of an unselected source")
	}
	if _, err := barrier.AcquireRead([]string{"source-b"}); err == nil {
		t.Fatal("two backups held the exclusive staging barrier")
	}
}

// A backup takes all of its sources or none. Locking only some of them would
// leave the rest free to change underneath the read-set.
func TestBarrierReadIsAllOrNothing(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	releaseMutation, err := barrier.AcquireMutation("source-b")
	if err != nil {
		t.Fatalf("acquire mutation: %v", err)
	}
	defer releaseMutation()

	if _, err := barrier.AcquireRead([]string{"source-a", "source-b"}); err == nil {
		t.Fatal("a partial read barrier was granted")
	}
	if barrier.BeingRead("source-a") {
		t.Fatal("a source was left locked by a refused read barrier")
	}
}

func TestBarrierMutationIsExclusive(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	release, err := barrier.AcquireMutation("source-a")
	if err != nil {
		t.Fatalf("acquire mutation: %v", err)
	}
	defer release()

	if _, err := barrier.AcquireMutation("source-a"); err == nil {
		t.Fatal("two collections were allowed to reconcile the same source")
	}
}

func TestBarrierReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	release, err := barrier.AcquireRead([]string{"source-a"})
	if err != nil {
		t.Fatalf("acquire read: %v", err)
	}
	release()
	release()

	if barrier.BeingRead("source-a") {
		t.Fatal("the source is still marked as being read after release")
	}
	// A double release must not have decremented a count another reader owns.
	first, err := barrier.AcquireRead([]string{"source-a"})
	if err != nil {
		t.Fatalf("acquire read again: %v", err)
	}
	release() // A stale release must not release the new owner.
	if !barrier.BeingRead("source-a") {
		t.Fatal("a stale release released the new reader's barrier")
	}
	first()
	if barrier.BeingRead("source-a") {
		t.Fatal("the source is still being read after every reader released")
	}
}

func TestBarrierRejectsEmptyReadSet(t *testing.T) {
	t.Parallel()

	if _, err := NewBarrier().AcquireRead(nil); err == nil {
		t.Fatal("an empty read barrier was granted")
	}
}

// Concurrent acquisition must never let two mutators hold the same source.
func TestBarrierMutationUnderContention(t *testing.T) {
	t.Parallel()

	barrier := NewBarrier()
	const attempts = 50

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	held := 0
	maximum := 0

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := barrier.AcquireMutation("source-a")
			if err != nil {
				return
			}
			mu.Lock()
			granted++
			held++
			if held > maximum {
				maximum = held
			}
			mu.Unlock()

			mu.Lock()
			held--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	if maximum > 1 {
		t.Fatalf("%d mutators held the same source at once", maximum)
	}
	if granted == 0 {
		t.Fatal("no mutator ever acquired the barrier")
	}
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	var typed *domain.Error
	if !domain.AsError(err, &typed) {
		t.Fatalf("error %v is not a typed ROTUP error", err)
	}
	return typed.Code
}
