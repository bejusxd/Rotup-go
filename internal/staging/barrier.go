package staging

import (
	"sync"

	"github.com/bejusxd/rotup/internal/domain"
)

// Barrier is the in-memory staging mutation barrier.
//
// It answers one question: may this operation touch these sources' staging
// trees right now? A collection reconciles a source's current tree and its
// revision area, so it needs exclusive access to that source. A backup reads
// several sources' trees for the whole duration of the restic scan, so it needs
// those sources to hold still. The two must never overlap, because a backup
// that reads a tree while reconciliation is moving files through it records a
// snapshot of a state that never existed.
//
// The backup barrier is daemon-wide and exclusive, as specified for v1. While
// it is held no collection may mutate staging, including unselected sources.
//
// This is a single-daemon in-memory lock and nothing more. It holds no database
// row, has no fencing epoch, and has no meaning across a restart: a process
// that dies releases everything it held, which is exactly right, because the
// operations it was protecting died with it. The general resource coordinator
// the architecture describes arrives with the scheduler.
//
// Acquisition never blocks. A caller that cannot have the sources it needs is
// told so immediately and fails cleanly, rather than queueing behind work whose
// duration nobody bounded. Waiting and coalescing belong to the scheduler
// milestone.
type Barrier struct {
	mu sync.Mutex
	// mutators holds the source IDs currently being reconciled. A source
	// appears at most once: mutation is exclusive.
	mutators map[string]struct{}
	reading  bool
}

// NewBarrier builds an empty barrier.
func NewBarrier() *Barrier {
	return &Barrier{
		mutators: make(map[string]struct{}),
	}
}

// AcquireMutation takes exclusive mutation rights on one source's staging tree.
// It fails when another mutation holds the source, or when a backup is reading
// it.
func (b *Barrier) AcquireMutation(sourceID string) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, taken := b.mutators[sourceID]; taken {
		return nil, domain.NewError(domain.ErrClassUnavailable, "staging_mutation_busy",
			"another operation is already reconciling this source's staging tree")
	}
	if b.reading {
		return nil, domain.NewError(domain.ErrClassUnavailable, "staging_read_in_progress",
			"a backup is currently reading this source's staging tree; collection cannot change it until the backup finishes")
	}
	b.mutators[sourceID] = struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.mutators, sourceID)
			b.mu.Unlock()
		})
	}, nil
}

// AcquireRead takes exclusive read rights on staging for
// the whole duration of a backup. It is all-or-nothing: a backup that could
// only lock some of its sources would read a partly frozen read-set, which is
// the situation this exists to prevent.
func (b *Barrier) AcquireRead(sourceIDs []string) (func(), error) {
	if len(sourceIDs) == 0 {
		return nil, domain.NewError(domain.ErrClassInvariant, "staging_read_empty",
			"a staging read barrier requires at least one source")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.mutators) != 0 {
		return nil, domain.NewError(domain.ErrClassUnavailable, "staging_mutation_in_progress",
			"a collection is currently changing staging; the backup cannot acquire the mutation barrier")
	}
	if b.reading {
		return nil, domain.NewError(domain.ErrClassUnavailable, "staging_read_in_progress",
			"another backup holds the staging barrier")
	}
	b.reading = true

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.reading = false
			b.mu.Unlock()
		})
	}, nil
}

// Mutating reports whether a source is currently being reconciled.
func (b *Barrier) Mutating(sourceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, taken := b.mutators[sourceID]
	return taken
}

// BeingRead reports whether a source's staging tree is currently being read by
// a backup.
func (b *Barrier) BeingRead(_ string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reading
}
