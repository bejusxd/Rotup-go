package backup

import (
	"context"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// SetupMode selects whether a repository is created or taken over.
type SetupMode string

const (
	// SetupInitialize creates a new restic repository. The location must be
	// empty: ROTUP never writes a repository over something it did not put
	// there.
	SetupInitialize SetupMode = "INITIALIZE"
	// SetupAdopt takes over an existing restic repository at ROTUP's own
	// repository path. It requires the password that already encrypts it.
	SetupAdopt SetupMode = "ADOPT"
)

// SetupInput is one explicit repository establishment request.
//
// The password is supplied by the administrator rather than generated. A
// generated password would have to be shown once and then be unrecoverable if
// that one display were missed, and building a way to show it again would mean
// ROTUP holding a recovery path for its own encryption, which is exactly the
// property that makes an off-host record worth having. Requiring the operator
// to bring a password they already recorded elsewhere makes the escrow the
// operator's, not ROTUP's, which is where it has to be.
type SetupInput struct {
	DiskID   string
	Mode     SetupMode
	Password string
	// EscrowAcknowledged is the administrator's explicit statement that the
	// password is recorded outside this host. Without it the request is
	// refused: a repository whose password exists only inside the installation
	// that is being backed up is not a backup anyone can restore from.
	EscrowAcknowledged bool
}

// StartRepositorySetup validates a repository establishment request, stores its
// password, and runs the operation in the background.
//
// The password is stored and then dropped here, before any background work
// starts, so plaintext never travels into a goroutine or lives for the duration
// of a mount.
func (s *Service) StartRepositorySetup(ctx context.Context, input SetupInput) (string, error) {
	if input.Mode != SetupInitialize && input.Mode != SetupAdopt {
		return "", domain.NewError(domain.ErrClassInvariant, "repository_mode_unknown",
			"unknown repository setup mode")
	}
	if !input.EscrowAcknowledged {
		return "", domain.NewError(domain.ErrClassConfiguration,
			domain.RepositoryErrorEscrowNotAcknowledged,
			"confirm that the repository password is recorded outside this host before establishing a repository; "+
				"restic cannot reset or recover it, and ROTUP will never show it again")
	}
	if err := domain.ValidateRepositoryPassword(input.Password); err != nil {
		return "", err
	}

	disk, err := s.store.Disk(ctx, input.DiskID)
	if err != nil {
		return "", err
	}
	if disk == nil {
		return "", domain.NewError(domain.ErrClassConfiguration, "disk_missing",
			"that disk is not registered")
	}
	if !disk.Enabled {
		return "", domain.NewError(domain.ErrClassConfiguration, "disk_disabled",
			"this disk is disabled; enable it before establishing a repository")
	}

	// Exclusivity precedes all metadata and password writes. A refused second
	// setup must never replace the password used by the first operation.
	lease, err := s.media.Acquire(ctx, disk.ID)
	if err != nil {
		return "", err
	}
	repository, err := s.resolveSetupTarget(ctx, *disk, input.Mode)
	if err != nil {
		lease.Release()
		return "", err
	}
	if err := s.storeRepositoryPassword(ctx, *repository, input.Password); err != nil {
		lease.Release()
		return "", err
	}
	if err := s.store.AcknowledgeRepositoryEscrow(ctx, repository.ID, s.now()); err != nil {
		lease.Release()
		return "", err
	}
	run, err := s.newRun(ctx, domain.RunKindDiskSetup, domain.RunTriggerManual, "requested")
	if err != nil {
		lease.Release()
		return "", err
	}
	s.event(ctx, domain.SeverityInfo, "repository.init_started", eventRefs{
		RepositoryID: repository.ID, DiskID: disk.ID, RunID: run.ID,
	}, map[string]any{"mode": string(input.Mode)})

	target := *repository
	mode := input.Mode
	s.run(func(background context.Context) {
		defer lease.Release()
		s.executeSetup(background, run, target, mode, lease)
	})
	return run.ID, nil
}

// resolveSetupTarget finds or creates the repository record this request is
// about, refusing a request that would overwrite an established repository.
func (s *Service) resolveSetupTarget(ctx context.Context, disk domain.Disk,
	mode SetupMode) (*domain.Repository, error) {

	existing, err := s.store.RepositoryForDisk(ctx, disk.ID)
	if err != nil {
		return nil, err
	}
	origin := domain.RepositoryInitialized
	if mode == SetupAdopt {
		origin = domain.RepositoryAdopted
	}

	if existing != nil {
		if existing.Established() {
			// A repository ROTUP has already proven is never re-established.
			// Re-initializing would destroy every snapshot on the media, and
			// re-adopting would let a fresh password quietly replace the one
			// that actually decrypts it.
			return nil, domain.NewError(domain.ErrClassConfiguration, "repository_already_established",
				"this disk already has a proven ROTUP repository; use recovery if it needs to be verified again")
		}
		if domain.NormalizeFilesystemUUID(existing.FilesystemUUID) !=
			domain.NormalizeFilesystemUUID(disk.FilesystemUUID) {
			return nil, domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorIdentityMismatch,
				"the existing repository record was established on a different filesystem UUID")
		}
		return existing, nil
	}

	id, err := domain.NewID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	repository := domain.Repository{
		ID:             id,
		DiskID:         disk.ID,
		FilesystemUUID: domain.NormalizeFilesystemUUID(disk.FilesystemUUID),
		RelativePath:   domain.RepositoryRelativePath,
		Origin:         origin,
		Health:         domain.RepositoryUninitialized,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateRepository(ctx, repository); err != nil {
		return nil, err
	}
	return &repository, nil
}

// storeRepositoryPassword puts the password in the secret store and points the
// repository at it, then deletes any material the change displaced.
func (s *Service) storeRepositoryPassword(ctx context.Context, repository domain.Repository, password string) error {
	locator, err := s.secrets.Put(ctx, domain.SecretPurposeRepositoryPassword, password)
	if err != nil {
		return err
	}
	refID, err := domain.NewID()
	if err != nil {
		return err
	}
	ref := domain.CredentialRef{
		ID:              refID,
		DisplayName:     "repository password",
		Provider:        s.secrets.Provider(),
		ProviderLocator: locator,
		Purpose:         domain.SecretPurposeRepositoryPassword,
	}
	displaced, err := s.store.AttachRepositoryCredential(ctx, repository.ID, ref)
	if err != nil {
		// The metadata row was not written, so the material it addresses is
		// unreachable and must not be left behind.
		if deleteErr := s.secrets.Delete(ctx, locator); deleteErr != nil {
			s.logger.Error("orphaned repository password material could not be removed", "error", deleteErr)
		}
		return err
	}
	if displaced != "" {
		if err := s.secrets.Delete(ctx, displaced); err != nil {
			s.logger.Error("displaced repository password material could not be removed", "error", err)
		}
	}
	s.event(ctx, domain.SeverityInfo, "repository.password_stored", eventRefs{
		RepositoryID: repository.ID, DiskID: repository.DiskID,
	}, map[string]any{"credential_ref_id": refID})
	return nil
}

// executeSetup runs the mount, initialize-or-adopt, verify, sync, unmount
// sequence.
//
// The repository only reaches HEALTHY at the end. restic reporting a created
// repository and ROTUP reading its identity back both happen while the disk is
// still mounted, and neither of them says the bytes are on the media. Only a
// completed flush and a confirmed unmount say that.
func (s *Service) executeSetup(ctx context.Context, run domain.Run, repository domain.Repository,
	mode SetupMode, lease ports.DiskLease) {

	run = s.startRun(ctx, run, "establishing the repository")
	refs := eventRefs{RepositoryID: repository.ID, DiskID: repository.DiskID, RunID: run.ID}

	if err := s.store.SetRepositoryHealth(ctx, repository.ID, domain.RepositoryInitializing, ""); err != nil {
		s.finishRunWithError(ctx, run, err, "the repository state could not be recorded")
		return
	}

	if err := lease.Mount(ctx); err != nil {
		// Nothing was written, so the repository is not suspect. It simply has
		// not been established.
		s.setHealthQuietly(ctx, repository.ID, domain.RepositoryUninitialized,
			"the disk could not be mounted, so no repository was established")
		s.finishRunWithError(ctx, run, err, "the disk could not be mounted")
		return
	}

	established, setupErr := s.establishOnMountedDisk(ctx, run, repository, mode, lease)

	// The disk is flushed and unmounted whatever happened. Leaving a repository
	// mounted after a failure would be the one outcome that is worse than the
	// failure itself.
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	unmountErr := lease.SyncAndUnmount(ctx)

	switch {
	case setupErr != nil:
		// init can write a partial repository before returning an error. Never
		// describe that location as untouched just because no ID was returned.
		s.setHealthQuietly(ctx, repository.ID, domain.RepositorySuspect, safeMessage(setupErr))
		s.event(ctx, domain.SeverityError, "repository.init_failed", refs,
			map[string]any{"reason": safeMessage(setupErr)})
		s.finishRunWithError(ctx, run, setupErr, "the repository could not be established")
		return

	case unmountErr != nil:
		// The repository exists and was proven readable, but nothing says the
		// bytes reached the media. That is exactly what SUSPECT means.
		s.setHealthQuietly(ctx, repository.ID, domain.RepositorySuspect,
			"the disk could not be flushed and unmounted after the repository was created, "+
				"so ROTUP cannot confirm the repository reached the media")
		s.event(ctx, domain.SeverityError, "repository.marked_suspect", refs,
			map[string]any{"reason": safeMessage(unmountErr)})
		s.finishRunWithError(ctx, run, unmountErr,
			"the disk could not be flushed and unmounted after the repository was created")
		return
	}

	if err := s.store.SetRepositoryHealth(ctx, repository.ID, domain.RepositoryHealthy, ""); err != nil {
		s.finishRunWithError(ctx, run, err, "the repository state could not be recorded")
		return
	}

	eventType := "repository.initialized"
	if mode == SetupAdopt {
		eventType = "repository.adopted"
	}
	s.event(ctx, domain.SeverityInfo, eventType, refs,
		map[string]any{"restic_repository_id": established})

	run.Status = domain.RunStatusSucceeded
	run.FinishedAt = s.now()
	run.Summary = "repository " + shortID(established) + " is ready on this disk"
	run.SummaryJSON = encodeSummary(map[string]any{
		"repository_id":        repository.ID,
		"disk_id":              repository.DiskID,
		"restic_repository_id": established,
		"mode":                 string(mode),
	})
	s.saveRun(ctx, run)
}

// establishOnMountedDisk does the repository work while the disk is mounted and
// its identity proven. It returns the restic repository identity it recorded.
func (s *Service) establishOnMountedDisk(ctx context.Context, run domain.Run,
	repository domain.Repository, mode SetupMode, lease ports.DiskLease) (string, error) {

	spec, err := s.repositorySpec(ctx, repository, lease)
	if err != nil {
		return "", err
	}

	state, err := inspectDirectory(spec.Directory)
	if err != nil {
		return "", err
	}
	switch mode {
	case SetupInitialize:
		switch state {
		case directoryAbsent, directoryEmpty:
		case directoryNotADirectory:
			return "", domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorPathOccupied,
				"something that is not a directory already occupies the repository location on this disk")
		default:
			return "", domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorPathOccupied,
				"the repository location on this disk is not empty; adopt the existing repository instead of "+
					"initializing over it")
		}
	case SetupAdopt:
		if state != directoryPopulated {
			// Adoption is only ever offered for a repository that is already
			// there. Adopting an empty location would mean ROTUP recording a
			// repository that does not exist.
			return "", domain.NewError(domain.ErrClassConfiguration, domain.RepositoryErrorNotEstablished,
				"there is no existing repository at ROTUP's repository location on this disk")
		}
	}

	var info ports.RepositoryInfo
	if mode == SetupInitialize {
		info, err = s.engine.InitializeRepository(ctx, spec)
	} else {
		info, err = s.engine.InspectRepository(ctx, spec)
	}
	if err != nil {
		return "", err
	}

	identity, err := domain.ValidateResticRepositoryID(info.ID)
	if err != nil {
		return "", err
	}
	if repository.Established() && repository.ResticRepositoryID != identity {
		return identity, domain.NewError(domain.ErrClassIntegrity, domain.RepositoryErrorIdentityMismatch,
			"the repository on this disk reports a different identity than the one ROTUP recorded")
	}

	// Reading the snapshot listing is what proves the repository is not merely
	// present but actually usable with the stored password. An adoption that
	// stopped at the configuration would accept a repository whose index cannot
	// be read.
	if _, err := s.engine.Snapshots(ctx, spec); err != nil {
		return identity, err
	}

	// The identity is recorded before the flush and unmount, still under
	// INITIALIZING. If the daemon dies here, the record says what was created
	// and says it is not yet proven, which is the truth.
	if err := s.store.SetRepositoryEstablished(ctx, repository.ID, identity,
		domain.RepositoryInitializing, s.now()); err != nil {
		return identity, err
	}
	s.logger.Info("repository established",
		"repository_id", repository.ID, "disk_id", repository.DiskID,
		"restic_repository_id", identity, "mode", string(mode), "run_id", run.ID)
	return identity, nil
}

// setHealthQuietly records a health transition, logging rather than failing when
// it cannot be written: the caller is already on a failure path and the original
// error is the one that matters.
func (s *Service) setHealthQuietly(ctx context.Context, repositoryID string,
	health domain.RepositoryHealth, reason string) {

	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := s.store.SetRepositoryHealth(ctx, repositoryID, health, reason); err != nil {
		s.logger.Error("the repository state could not be recorded",
			"repository_id", repositoryID, "error", err)
	}
}

// RepositoryView is the read model behind the repository pages.
type RepositoryView struct {
	Repository domain.Repository
	Disk       *domain.Disk
	// Connected reports whether the repository's disk is currently present.
	Connected bool
	// Busy reports whether a repository operation is running for this disk.
	Busy bool
	// HasPassword reports whether a repository password is stored. It never
	// says anything about the password itself.
	HasPassword bool
}

// ListRepositories builds the repository list view.
func (s *Service) ListRepositories(ctx context.Context) ([]RepositoryView, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]RepositoryView, 0, len(repositories))
	for _, repository := range repositories {
		view, err := s.buildView(ctx, repository)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// RepositoryDetail builds the single repository view.
func (s *Service) RepositoryDetail(ctx context.Context, repositoryID string) (RepositoryView, error) {
	repository, err := s.store.Repository(ctx, repositoryID)
	if err != nil {
		return RepositoryView{}, err
	}
	if repository == nil {
		return RepositoryView{}, domain.NewError(domain.ErrClassConfiguration, "repository_missing",
			"that repository does not exist")
	}
	return s.buildView(ctx, *repository)
}

func (s *Service) buildView(ctx context.Context, repository domain.Repository) (RepositoryView, error) {
	view := RepositoryView{Repository: repository}
	disk, err := s.store.Disk(ctx, repository.DiskID)
	if err != nil {
		return RepositoryView{}, err
	}
	view.Disk = disk
	if disk != nil {
		view.Connected = disk.CurrentStatus != domain.DiskDisconnected
		view.Busy = disk.CurrentStatus.Busy()
	}
	ref, err := s.store.RepositoryCredentialRef(ctx, repository.ID)
	if err != nil {
		return RepositoryView{}, err
	}
	view.HasPassword = ref.ProviderLocator != ""
	return view, nil
}

// BackupHistory returns a repository's backup runs with the durable run rows.
func (s *Service) BackupHistory(ctx context.Context, repositoryID string, limit int) ([]BackupHistoryEntry, error) {
	backups, err := s.store.ListBackupRunsForRepository(ctx, repositoryID, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]BackupHistoryEntry, 0, len(backups))
	for _, item := range backups {
		entry := BackupHistoryEntry{Backup: item}
		run, err := s.store.Run(ctx, item.RunID)
		if err != nil {
			return nil, err
		}
		if run != nil {
			entry.Run = *run
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// BackupHistoryEntry pairs a backup's durable detail with its run row.
type BackupHistoryEntry struct {
	Run    domain.Run
	Backup domain.BackupRun
}

// Age reports how long ago the backup finished.
func (e BackupHistoryEntry) Age(now time.Time) time.Duration {
	if e.Run.FinishedAt.IsZero() {
		return 0
	}
	return now.Sub(e.Run.FinishedAt)
}
