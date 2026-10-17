package coordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/staging"
)

type resources struct {
	mu         sync.Mutex
	barrier    *staging.Barrier
	sources    map[string]bool
	disks      map[string]bool
	wrongOrder bool
	diskErr    error
	onAcquire  func()
}

func fixture() (*Coordinator, *resources) {
	r := &resources{barrier: staging.NewBarrier(), sources: map[string]bool{}, disks: map[string]bool{}}
	return New(r.barrier, r, r), r
}

func (r *resources) TryAcquireSource(id string) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.barrier.Mutating(id) {
		r.wrongOrder = true
	}
	if r.sources[id] {
		return nil, domain.NewError(domain.ErrClassUnavailable, "source_busy", "busy")
	}
	r.sources[id] = true
	return func() { r.mu.Lock(); defer r.mu.Unlock(); delete(r.sources, id) }, nil
}

func (r *resources) Acquire(_ context.Context, id string) (ports.DiskLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.barrier.BeingRead("") {
		r.wrongOrder = true
	}
	if r.diskErr != nil {
		return nil, r.diskErr
	}
	if r.disks[id] {
		return nil, domain.NewError(domain.ErrClassUnavailable, "disk_busy", "busy")
	}
	r.disks[id] = true
	if r.onAcquire != nil {
		r.onAcquire()
	}
	return &diskLease{id: id, release: func() { r.mu.Lock(); defer r.mu.Unlock(); delete(r.disks, id) }}, nil
}

type diskLease struct {
	ports.DiskLease // Mount operations are deliberately not implemented by the lock fake.
	id              string
	release         func()
}

func (l *diskLease) DiskID() string { return l.id }
func (l *diskLease) Release()       { l.release() }

func TestCollectionsOverlapButBackupExcludesAllMutation(t *testing.T) {
	t.Parallel()
	c, r := fixture()
	ctx := context.Background()
	a, err := c.TryCollection(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.TryCollection(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TryCollection(ctx, "a"); !Busy(err) {
		t.Fatalf("same source was not busy: %v", err)
	}
	if _, err := c.TryBackup(ctx, []string{"unselected"}, "disk"); !Busy(err) {
		t.Fatalf("backup overlap: %v", err)
	}
	a()
	b()
	lease, err := c.TryBackup(ctx, []string{"a"}, "disk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TryCollection(ctx, "unselected"); !Busy(err) {
		t.Fatalf("unselected mutation: %v", err)
	}
	lease.Release()
	if r.wrongOrder {
		t.Fatal("resources were acquired out of order")
	}
}

func TestPartialAcquisitionRollsBackAndStaleReleaseCannotUnlockNewOwner(t *testing.T) {
	t.Parallel()
	c, r := fixture()
	ctx := context.Background()
	r.sources["a"] = true
	if _, err := c.TryCollection(ctx, "a"); !Busy(err) {
		t.Fatalf("source refusal: %v", err)
	}
	if r.barrier.Mutating("a") {
		t.Fatal("refused source retained staging")
	}
	delete(r.sources, "a")
	r.disks["disk"] = true
	if _, err := c.TryBackup(ctx, []string{"a"}, "disk"); !Busy(err) {
		t.Fatalf("disk refusal: %v", err)
	}
	if r.barrier.BeingRead("") {
		t.Fatal("refused disk retained staging")
	}
	delete(r.disks, "disk")
	old, err := c.TryBackup(ctx, []string{"a"}, "disk")
	if err != nil {
		t.Fatal(err)
	}
	old.Release()
	current, err := c.TryBackup(ctx, []string{"a"}, "disk")
	if err != nil {
		t.Fatal(err)
	}
	old.Release()
	old.ReleaseStaging()
	if !r.disks["disk"] || !r.barrier.BeingRead("") {
		t.Fatal("stale release unlocked new owner")
	}
	current.ReleaseStaging()
	if !r.disks["disk"] || r.barrier.BeingRead("") {
		t.Fatal("staging release must keep disk exclusive")
	}
	release, err := c.TryCollection(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	current.Release()
}

func TestWaitingIsCancellableAndHoldsNoPartialResource(t *testing.T) {
	t.Parallel()
	for _, backup := range []bool{false, true} {
		c, r := fixture()
		r.sources["a"], r.disks["disk"] = true, true
		ctx, cancel := context.WithCancel(context.Background())
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			if backup {
				_, err := c.AcquireBackup(ctx, []string{"a"}, "disk")
				done <- err
			} else {
				_, err := c.AcquireCollection(ctx, "a")
				done <- err
			}
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancellation leaked a waiter")
		}
		if r.barrier.Mutating("a") || r.barrier.BeingRead("") {
			t.Fatal("cancelled waiter retained staging")
		}
	}
}

func TestCancellationAfterDiskAcquisitionReleasesEverything(t *testing.T) {
	t.Parallel()
	c, r := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.onAcquire = cancel
	if _, err := c.TryBackup(ctx, []string{"a"}, "disk"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r.disks["disk"] || r.barrier.BeingRead("") {
		t.Fatal("late cancellation retained ownership")
	}
}

func TestOnlyContentionIsRetried(t *testing.T) {
	t.Parallel()
	c, r := fixture()
	r.diskErr = domain.NewError(domain.ErrClassUnavailable, "mount_helper_device_not_found", "absent")
	if Busy(r.diskErr) {
		t.Fatal("media absence was treated as in-memory contention")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.AcquireBackup(ctx, []string{"a"}, "disk"); err != r.diskErr {
		t.Fatalf("underlying error was hidden: %v", err)
	}
	if r.barrier.BeingRead("") {
		t.Fatal("failed acquisition retained staging")
	}
}

func TestContendedAcquisitionEventuallyCompletesWithoutDeadlock(t *testing.T) {
	t.Parallel()
	c, r := fixture()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%3 == 0 {
				lease, err := c.AcquireBackup(ctx, []string{"a", "b"}, "disk")
				if err != nil {
					errs <- err
					return
				}
				lease.Release()
			} else {
				id := "a"
				if i%3 == 2 {
					id = "b"
				}
				release, err := c.AcquireCollection(ctx, id)
				if err != nil {
					errs <- err
					return
				}
				release()
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if r.wrongOrder || len(r.sources) != 0 || len(r.disks) != 0 || r.barrier.BeingRead("") {
		t.Fatal("resource order or cleanup invariant failed")
	}
}
