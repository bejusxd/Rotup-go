package removable

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

const component = "removable"

// Store is the narrow repository seam the media service needs.
type Store interface {
	ListDisks(ctx context.Context) ([]domain.Disk, error)
	Disk(ctx context.Context, id string) (*domain.Disk, error)
	DiskByFilesystemUUID(ctx context.Context, filesystemUUID string) (*domain.Disk, error)
	CreateDisk(ctx context.Context, disk domain.Disk) error
	UpdateDiskSettings(ctx context.Context, id, name string, enabled bool) error
	SaveDiskState(ctx context.Context, state domain.DiskState) error
	AppendEvent(ctx context.Context, event domain.Event) error
}

// Service implements the M3 removable-media use cases.
type Service struct {
	store      Store
	enumerator ports.MediaEnumerator
	mounts     ports.MountManager
	logger     *slog.Logger
	now        func() time.Time

	// locks serialize media operations per disk. M3 is a single daemon, so an
	// in-memory lock is the whole coordination requirement; the architecture's
	// resource coordinator arrives with the scheduler.
	locks *keyedLocks

	// Serialize connection observations and their persistence. Snapshot and
	// mount identity checks never update this history. Values are connection
	// epochs, not polling, registration or operation-completion timestamps.
	mediaMu                sync.Mutex
	knownMedia             map[string]time.Time
	connectionsInitialized bool
}

// Options configures the service.
type Options struct {
	Store      Store
	Enumerator ports.MediaEnumerator
	Mounts     ports.MountManager
	Logger     *slog.Logger
	Now        func() time.Time
}

// New builds the removable-media service.
func New(options Options) *Service {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		store:      options.Store,
		enumerator: options.Enumerator,
		mounts:     options.Mounts,
		logger:     logger.With("component", component),
		now:        now,
		locks:      newKeyedLocks(),
		knownMedia: make(map[string]time.Time),
	}
}

// PlatformAvailable reports whether this platform can manage media at all.
func (s *Service) PlatformAvailable() bool { return s.mounts.Available() }

// Busy reports whether a media operation is in progress for a disk.
func (s *Service) Busy(diskID string) bool { return s.locks.held(diskID) }

// observation is one authoritative pass over the system's current state.
type observation struct {
	Media  []domain.ObservedMedia
	Mounts []domain.MountObservation
}

// observe takes a fresh authoritative reading. Both halves are re-read every
// time; nothing is cached, because a cached device node is exactly the kind of
// stale fact that leads to writing to the wrong disk.
func (s *Service) observe(ctx context.Context) (observation, error) {
	media, err := s.enumerator.Enumerate(ctx)
	if err != nil {
		return observation{}, err
	}
	mounts, err := s.mounts.Observe(ctx)
	if err != nil {
		return observation{}, err
	}
	return observation{Media: media, Mounts: mounts}, nil
}

// Reconcile brings every registered disk's persisted state in line with what
// the system currently presents. It is the authoritative convergence step: a
// missed hotplug hint cannot lose a disk permanently, because the next
// reconciliation finds it again.
//
// Disks with a media operation in flight are skipped: their status belongs to
// that operation until it finishes.
func (s *Service) Reconcile(ctx context.Context) error {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	current, err := s.observe(ctx)
	if err != nil {
		return err
	}
	disks, err := s.store.ListDisks(ctx)
	if err != nil {
		return err
	}

	s.reportMediaChanges(ctx, current.Media, disks)

	for _, disk := range disks {
		if s.locks.held(disk.ID) {
			continue
		}
		expected, _ := s.mounts.MountPointFor(disk.ID)
		result := evaluate(disk, current.Media, current.Mounts, expected)
		if err := s.applyEvaluation(ctx, disk, result); err != nil {
			return err
		}
	}
	return nil
}

// applyEvaluation persists a derived status, but only when something actually
// changed. Writing an unchanged row on every poll would turn the event log into
// a stream of noise.
func (s *Service) applyEvaluation(ctx context.Context, disk domain.Disk, result evaluation) error {
	connectedAt, present := s.knownMedia[disk.FilesystemUUID]
	connectionChanged := present && (disk.LastConnectedAt == nil || !disk.LastConnectedAt.Equal(connectedAt))
	statusChanged := disk.CurrentStatus != result.Status || disk.LastErrorCode != result.ErrorCode
	if !statusChanged && !connectionChanged {
		return nil
	}
	state := domain.DiskState{
		ID:        disk.ID,
		Status:    result.Status,
		ErrorCode: result.ErrorCode,
	}
	now := s.now()
	if connectionChanged {
		state.LastConnectedAt = &connectedAt
	}
	if result.Status == domain.DiskDisconnected && disk.CurrentStatus != domain.DiskDisconnected {
		state.LastDisconnectedAt = &now
	}
	if err := s.store.SaveDiskState(ctx, state); err != nil {
		s.logger.Error("disk state could not be persisted", "disk_id", disk.ID, "error", err)
		return err
	}
	if !statusChanged {
		return nil
	}

	s.logger.Info("disk state changed",
		"disk_id", disk.ID, "from", string(disk.CurrentStatus), "to", string(result.Status),
		"error_code", result.ErrorCode)

	switch {
	case result.Ambiguous:
		s.event(ctx, domain.SeverityError, "disk.duplicate_uuid", disk.ID,
			map[string]any{"filesystem_uuid": disk.FilesystemUUID})
	case result.ErrorCode == domain.DiskErrorExternalMount:
		s.event(ctx, domain.SeverityWarning, "disk.external_mount", disk.ID,
			map[string]any{"mount_points": result.ExternalMounts})
	case result.Status == domain.DiskDisconnected:
		s.event(ctx, domain.SeverityInfo, "media.removed", disk.ID, nil)
	case result.Status == domain.DiskAvailableUnmounted && disk.CurrentStatus == domain.DiskDisconnected:
		s.event(ctx, domain.SeverityInfo, "media.detected", disk.ID, nil)
	case result.Status == domain.DiskError:
		s.event(ctx, domain.SeverityError, "disk.error", disk.ID,
			map[string]any{"error_code": result.ErrorCode})
	}
	return nil
}

// reportMediaChanges records observed connections, including unregistered media
// and busy disks. Caller holds mediaMu across observation and persistence. At
// startup preserve the last known epoch of an already-connected disk: physical
// connection order while the daemon was stopped cannot be inferred. Filesystems
// first seen in one pass tie; device names and enumeration order are irrelevant.
func (s *Service) reportMediaChanges(ctx context.Context, media []domain.ObservedMedia, disks []domain.Disk) {
	previous := s.knownMedia
	seed := make(map[string]time.Time)
	if !s.connectionsInitialized {
		for _, disk := range disks {
			if disk.CurrentStatus != domain.DiskDisconnected && disk.LastConnectedAt != nil &&
				(disk.LastDisconnectedAt == nil || disk.LastDisconnectedAt.Before(*disk.LastConnectedAt)) {
				seed[disk.FilesystemUUID] = *disk.LastConnectedAt
			}
		}
	}
	now := s.now()
	present := make(map[string]time.Time, len(media))
	for _, item := range media {
		uuid := domain.NormalizeFilesystemUUID(item.FilesystemUUID)
		if uuid != "" {
			connectedAt, exists := previous[uuid]
			if !exists {
				connectedAt = now
				if prior, ok := seed[uuid]; ok {
					connectedAt = prior
				}
			}
			present[uuid] = connectedAt
		}
	}
	ambiguous := ambiguousUUIDs(media)

	s.knownMedia = present
	s.connectionsInitialized = true

	for uuid := range present {
		if _, existed := previous[uuid]; !existed {
			severity := domain.SeverityInfo
			payload := map[string]any{"filesystem_uuid": uuid}
			if count, duplicated := ambiguous[uuid]; duplicated {
				severity = domain.SeverityError
				payload["observations"] = count
				s.event(ctx, severity, "media.duplicate_uuid", "", payload)
				continue
			}
			s.event(ctx, severity, "media.detected", "", payload)
		}
	}
	for uuid := range previous {
		if _, exists := present[uuid]; !exists {
			s.event(ctx, domain.SeverityInfo, "media.removed", "",
				map[string]any{"filesystem_uuid": uuid})
		}
	}
}

// MediaView is one row of the disks page: an observation, a registration, or
// both.
type MediaView struct {
	Observed       *domain.ObservedMedia
	Disk           *domain.Disk
	Status         domain.DiskStatus
	ErrorCode      string
	Ambiguous      bool
	Supported      bool
	Busy           bool
	MountPoint     string
	ExternalMounts []string
}

// Registered reports whether this row corresponds to a registered disk.
func (v MediaView) Registered() bool { return v.Disk != nil }

// Snapshot is the read model behind the disks page.
type Snapshot struct {
	Views             []MediaView
	GeneratedAt       time.Time
	PlatformAvailable bool
	PlatformReason    string
	// EnumerationError is a safe message when the system could not be read at
	// all. The page still renders registered disks so their last known state
	// stays visible.
	EnumerationError string
}

// Snapshot builds the current view of media and registered disks. It performs
// no state transitions, so refreshing a page never changes anything.
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot := Snapshot{
		GeneratedAt:       s.now(),
		PlatformAvailable: s.mounts.Available(),
	}
	if !snapshot.PlatformAvailable {
		snapshot.PlatformReason = "ROTUP manages removable media on Linux only; this platform is for development"
	}

	disks, err := s.store.ListDisks(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	current, err := s.observe(ctx)
	if err != nil {
		snapshot.EnumerationError = safeMessage(err)
	}
	ambiguous := ambiguousUUIDs(current.Media)

	registeredByUUID := make(map[string]domain.Disk, len(disks))
	for _, disk := range disks {
		registeredByUUID[disk.FilesystemUUID] = disk
	}

	// One row per registered disk, whether or not it is currently present.
	for _, disk := range disks {
		expected, _ := s.mounts.MountPointFor(disk.ID)
		result := evaluate(disk, current.Media, current.Mounts, expected)
		copyOfDisk := disk
		view := MediaView{
			Disk:           &copyOfDisk,
			Status:         result.Status,
			ErrorCode:      result.ErrorCode,
			Ambiguous:      result.Ambiguous,
			Observed:       result.Observed,
			MountPoint:     result.MountPoint,
			ExternalMounts: result.ExternalMounts,
			Busy:           s.locks.held(disk.ID),
			Supported:      domain.FilesystemSupported(disk.FilesystemType),
		}
		// While an operation is running the persisted transient status is the
		// truthful answer; the derived one describes a moment that has passed.
		if view.Busy && disk.CurrentStatus.Busy() {
			view.Status = disk.CurrentStatus
		}
		if result.Observed != nil {
			view.Supported = domain.FilesystemSupported(result.Observed.FilesystemType)
		}
		snapshot.Views = append(snapshot.Views, view)
	}

	// One row per observed but unregistered filesystem. These are observation
	// only: nothing about this row offers a mount.
	for i := range current.Media {
		observed := current.Media[i]
		if observed.FilesystemUUID != "" {
			if _, registered := registeredByUUID[observed.FilesystemUUID]; registered {
				continue
			}
		}
		_, duplicated := ambiguous[observed.FilesystemUUID]
		snapshot.Views = append(snapshot.Views, MediaView{
			Observed:  &observed,
			Status:    domain.DiskDetected,
			Ambiguous: duplicated,
			Supported: domain.FilesystemSupported(observed.FilesystemType),
		})
	}

	sort.SliceStable(snapshot.Views, func(i, j int) bool {
		left, right := snapshot.Views[i], snapshot.Views[j]
		if left.Registered() != right.Registered() {
			return left.Registered()
		}
		return viewLabel(left) < viewLabel(right)
	})
	return snapshot, nil
}

func viewLabel(view MediaView) string {
	if view.Disk != nil {
		return view.Disk.Name
	}
	if view.Observed != nil {
		return view.Observed.DevicePath
	}
	return ""
}

func (s *Service) event(ctx context.Context, severity, eventType, diskID string, payload map[string]any) {
	encoded := "{}"
	if payload != nil {
		if raw, err := json.Marshal(payload); err == nil {
			encoded = string(raw)
		}
	}
	event := domain.Event{
		OccurredAt:  s.now(),
		Severity:    severity,
		EventType:   eventType,
		Component:   component,
		DiskID:      diskID,
		PayloadJSON: encoded,
	}
	if err := s.store.AppendEvent(ctx, event); err != nil {
		s.logger.Error("event could not be persisted", "event_type", eventType, "error", err)
	}
}

func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	var typed *domain.Error
	if domain.AsError(err, &typed) {
		return typed.SafeMessage()
	}
	return "the system's block devices could not be read"
}

// keyedLocks serializes media operations per disk.
type keyedLocks struct {
	mu   sync.Mutex
	busy map[string]struct{}
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{busy: make(map[string]struct{})}
}

func (k *keyedLocks) tryAcquire(key string) (func(), bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, taken := k.busy[key]; taken {
		return nil, false
	}
	k.busy[key] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			delete(k.busy, key)
			k.mu.Unlock()
		})
	}, true
}

func (k *keyedLocks) held(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, taken := k.busy[key]
	return taken
}

func busyError() error {
	return domain.NewError(domain.ErrClassUnavailable, "disk_busy",
		"another media operation is already running for this disk")
}
