package removable

import (
	"context"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
)

// Register records a currently observed filesystem as a ROTUP backup disk.
//
// Registration is metadata only. It never mounts, never writes to the media,
// never formats or repartitions, and never changes a filesystem UUID. Unknown
// media stays observe-only until an administrator does exactly this.
func (s *Service) Register(ctx context.Context, filesystemUUID, name string) (string, error) {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	uuid, err := domain.ValidateFilesystemUUID(filesystemUUID)
	if err != nil {
		return "", err
	}
	diskName, err := domain.ValidateDiskName(name)
	if err != nil {
		return "", err
	}

	existing, err := s.store.DiskByFilesystemUUID(ctx, uuid)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", domain.NewError(domain.ErrClassConfiguration, "disk_already_registered",
			"a disk with that filesystem UUID is already registered")
	}

	current, err := s.observe(ctx)
	if err != nil {
		return "", err
	}
	matches := matchingMedia(current.Media, uuid)
	switch len(matches) {
	case 1:
	case 0:
		return "", domain.NewError(domain.ErrClassUnavailable, "media_not_observed",
			"no connected filesystem currently carries that UUID, so it cannot be registered")
	default:
		// Registering while the identity is ambiguous would record a UUID that
		// does not uniquely name a disk.
		return "", domain.NewError(domain.ErrClassIntegrity, "ambiguous_identity",
			"more than one connected filesystem claims that UUID; disconnect the duplicate before registering")
	}

	observed := matches[0]
	if !domain.FilesystemSupported(observed.FilesystemType) {
		return "", domain.NewError(domain.ErrClassConfiguration, "unsupported_filesystem",
			"ROTUP does not manage "+observed.FilesystemType+" media; supported filesystems are "+
				supportedList())
	}
	disks, err := s.store.ListDisks(ctx)
	if err != nil {
		return "", err
	}
	s.reportMediaChanges(ctx, current.Media, disks)
	connectedAt := s.knownMedia[uuid]

	id, err := domain.NewID()
	if err != nil {
		return "", err
	}
	now := s.now()
	disk := domain.Disk{
		ID:                id,
		Name:              diskName,
		FilesystemUUID:    uuid,
		FilesystemType:    observed.FilesystemType,
		Enabled:           true,
		FirstRegisteredAt: now,
		LastConnectedAt:   &connectedAt,
		CurrentStatus:     domain.DiskAvailableUnmounted,
		UpdatedAt:         now,
	}
	if err := s.store.CreateDisk(ctx, disk); err != nil {
		return "", err
	}
	s.event(ctx, domain.SeverityInfo, "disk.registered", id, map[string]any{
		"filesystem_uuid": uuid,
		"filesystem_type": observed.FilesystemType,
		"name":            diskName,
	})
	return id, nil
}

// UpdateSettings changes only the friendly metadata an administrator owns.
func (s *Service) UpdateSettings(ctx context.Context, diskID, name string, enabled bool) error {
	diskName, err := domain.ValidateDiskName(name)
	if err != nil {
		return err
	}
	disk, err := s.requireDisk(ctx, diskID)
	if err != nil {
		return err
	}
	if err := s.store.UpdateDiskSettings(ctx, disk.ID, diskName, enabled); err != nil {
		return err
	}
	s.event(ctx, domain.SeverityInfo, "disk.updated", disk.ID,
		map[string]any{"name": diskName, "enabled": enabled})
	return nil
}

// Mount performs the explicit mount operation.
//
// The order of the checks is the safety property. Identity is resolved from a
// fresh enumeration, the filesystem must be one ROTUP supports, nothing may
// already be mounted from that device, and — the step that matters most — the
// disk is only marked READY after the mounted filesystem has been read back and
// proven to be the registered one. A mount command that exits successfully is
// never on its own accepted as proof.
func (s *Service) Mount(ctx context.Context, diskID string) error {
	release, ok := s.locks.tryAcquire(diskID)
	if !ok {
		return busyError()
	}
	defer release()
	return s.mount(ctx, diskID)
}

// mount performs the mount with the disk's lock already held. A repository
// operation holds that lock for its whole span through a Lease, so it calls
// this rather than Mount.
func (s *Service) mount(ctx context.Context, diskID string) error {
	disk, err := s.requireDisk(ctx, diskID)
	if err != nil {
		return err
	}
	if !disk.Enabled {
		return domain.NewError(domain.ErrClassConfiguration, "disk_disabled",
			"this disk is disabled; enable it before mounting")
	}
	expected, err := s.mounts.MountPointFor(disk.ID)
	if err != nil {
		return err
	}

	s.setStatus(ctx, disk.ID, domain.DiskValidating, "")
	s.event(ctx, domain.SeverityInfo, "disk.mount_started", disk.ID, nil)

	observed, err := s.resolveForOperation(ctx, *disk)
	if err != nil {
		s.failOperation(ctx, disk.ID, err)
		return err
	}
	if !domain.FilesystemSupported(observed.FilesystemType) {
		failure := domain.NewError(domain.ErrClassConfiguration, domain.DiskErrorUnsupportedFilesystem,
			"ROTUP does not mount "+observed.FilesystemType+" media")
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorUnsupportedFilesystem)
		s.event(ctx, domain.SeverityError, "disk.mount_failed", disk.ID,
			map[string]any{"error_code": domain.DiskErrorUnsupportedFilesystem})
		return failure
	}

	// Anything already mounted from this device — including at ROTUP's own
	// mount point after an unclean shutdown — is a condition to report, not to
	// mount on top of.
	current, err := s.observe(ctx)
	if err != nil {
		s.failOperation(ctx, disk.ID, err)
		return err
	}
	if existing := mountsForDevice(current.Mounts, observed.DevicePath); len(existing) > 0 {
		code := domain.DiskErrorExternalMount
		failure := domain.NewError(domain.ErrClassIntegrity, code,
			"this filesystem is already mounted at "+existing[0].MountPoint+
				"; ROTUP will not mount over a mount it does not own")
		s.setStatus(ctx, disk.ID, domain.DiskError, code)
		s.event(ctx, domain.SeverityWarning, "disk.mount_failed", disk.ID,
			map[string]any{"error_code": code, "mount_point": existing[0].MountPoint})
		return failure
	}

	s.setStatus(ctx, disk.ID, domain.DiskMounting, "")
	result, err := s.mounts.Mount(ctx, ports.MountRequest{
		DiskID:         disk.ID,
		FilesystemUUID: disk.FilesystemUUID,
		FilesystemType: observed.FilesystemType,
	})
	if err != nil {
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorMountFailed)
		s.event(ctx, domain.SeverityError, "disk.mount_failed", disk.ID,
			map[string]any{"error_code": domain.DiskErrorMountFailed, "reason": safeMessage(err)})
		return err
	}

	if verifyErr := s.verifyMountedIdentity(ctx, *disk, expected, result); verifyErr != nil {
		// The mount succeeded mechanically but is not provably the right disk.
		// Leaving it mounted would leave an unidentified filesystem writable
		// under ROTUP's own mount root, so it is removed immediately.
		if unmountErr := s.mounts.Unmount(ctx, disk.ID); unmountErr != nil {
			s.logger.Error("an unverified mount could not be removed",
				"disk_id", disk.ID, "error", unmountErr)
		}
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorIdentityMismatch)
		s.event(ctx, domain.SeverityCritical, "disk.identity_mismatch", disk.ID,
			map[string]any{"reason": safeMessage(verifyErr)})
		return verifyErr
	}

	s.setStatus(ctx, disk.ID, domain.DiskReady, "")
	s.event(ctx, domain.SeverityInfo, "disk.mounted", disk.ID,
		map[string]any{"mount_point": expected})
	return nil
}

// verifyMountedIdentity proves that what is now mounted at ROTUP's mount point
// really is the registered filesystem.
//
// It deliberately re-reads both the kernel mount table and the block device
// list rather than trusting the mount call's own report. The chain it
// establishes is: a mount exists at the expected point, its source device is
// currently observed, and that device carries the registered UUID.
//
// Marker verification would slot in here as one further check once a marker
// exists; M3 registers no marker, so identity rests on the UUID.
func (s *Service) verifyMountedIdentity(ctx context.Context, disk domain.Disk,
	expected string, result ports.MountResult) error {

	if result.FilesystemUUID != "" && result.FilesystemUUID != disk.FilesystemUUID {
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"the mounted filesystem does not carry the registered UUID")
	}

	current, err := s.observe(ctx)
	if err != nil {
		return domain.WrapError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"the mounted filesystem's identity could not be confirmed", err)
	}

	var mounted *domain.MountObservation
	for i := range current.Mounts {
		if current.Mounts[i].MountPoint == expected {
			mounted = &current.Mounts[i]
			break
		}
	}
	if mounted == nil {
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"nothing is mounted at the expected mount point after mounting")
	}

	var source *domain.ObservedMedia
	for i := range current.Media {
		if current.Media[i].DevicePath == mounted.Source {
			source = &current.Media[i]
			break
		}
	}
	if source == nil {
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"the device mounted at the expected mount point could not be identified")
	}
	if domain.NormalizeFilesystemUUID(source.FilesystemUUID) != disk.FilesystemUUID {
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"the filesystem mounted at the expected mount point carries a different UUID")
	}
	if domain.NormalizeFilesystemType(mounted.FilesystemType) !=
		domain.NormalizeFilesystemType(disk.FilesystemType) {
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorIdentityMismatch,
			"the filesystem mounted at the expected mount point is not the registered type")
	}
	return nil
}

// SyncAndUnmount flushes the filesystem and then removes ROTUP's mount,
// verifying from the kernel mount table that it is really gone.
//
// Nothing here reports a disk as safe to remove on the strength of a command's
// exit status. A failed flush, a failed unmount, or a device that vanished
// mid-operation each end in a state that says so.
func (s *Service) SyncAndUnmount(ctx context.Context, diskID string) error {
	release, ok := s.locks.tryAcquire(diskID)
	if !ok {
		return busyError()
	}
	defer release()
	return s.syncAndUnmount(ctx, diskID)
}

// syncAndUnmount performs the flush and unmount with the disk's lock already
// held, for a repository operation holding a Lease.
func (s *Service) syncAndUnmount(ctx context.Context, diskID string) error {
	disk, err := s.requireDisk(ctx, diskID)
	if err != nil {
		return err
	}
	expected, err := s.mounts.MountPointFor(disk.ID)
	if err != nil {
		return err
	}

	s.setStatus(ctx, disk.ID, domain.DiskSyncing, "")
	s.event(ctx, domain.SeverityInfo, "disk.sync_started", disk.ID, nil)

	syncErr := s.mounts.Sync(ctx, disk.ID)
	if syncErr != nil {
		// The flush failed, so the media is not safe to remove. The mount is
		// still cleaned up if it can be, because leaving an unusable
		// filesystem mounted helps nobody, but the recorded state stays an
		// error either way.
		if unmountErr := s.mounts.Unmount(ctx, disk.ID); unmountErr != nil {
			s.logger.Error("the mount could not be removed after a failed flush",
				"disk_id", disk.ID, "error", unmountErr)
		}
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorSyncFailed)
		s.event(ctx, domain.SeverityError, "disk.sync_failed", disk.ID,
			map[string]any{"reason": safeMessage(syncErr)})
		return syncErr
	}

	s.setStatus(ctx, disk.ID, domain.DiskUnmounting, "")
	if err := s.mounts.Unmount(ctx, disk.ID); err != nil {
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorUnmountFailed)
		s.event(ctx, domain.SeverityError, "disk.unmount_failed", disk.ID,
			map[string]any{"reason": safeMessage(err)})
		return err
	}

	// The unmount is only believed once the kernel mount table agrees.
	current, err := s.observe(ctx)
	if err != nil {
		s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorUnmountFailed)
		s.event(ctx, domain.SeverityError, "disk.unmount_failed", disk.ID,
			map[string]any{"reason": "the mount table could not be re-read to confirm the unmount"})
		return domain.WrapError(domain.ErrClassIntegrity, domain.DiskErrorUnmountFailed,
			"the unmount could not be confirmed", err)
	}
	for _, mount := range current.Mounts {
		if mount.MountPoint == expected {
			failure := domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorUnmountFailed,
				"the filesystem is still mounted at the ROTUP mount point after unmounting")
			s.setStatus(ctx, disk.ID, domain.DiskError, domain.DiskErrorUnmountFailed)
			s.event(ctx, domain.SeverityError, "disk.unmount_failed", disk.ID,
				map[string]any{"error_code": domain.DiskErrorUnmountFailed})
			return failure
		}
	}

	// A device that disappeared during the operation must not be reported as
	// safely removed: nothing was flushed to hardware that is no longer there.
	if len(matchingMedia(current.Media, disk.FilesystemUUID)) == 0 {
		s.setStatus(ctx, disk.ID, domain.DiskDisconnected, domain.DiskErrorRemovedDuringOperation)
		s.event(ctx, domain.SeverityWarning, "disk.removed_during_operation", disk.ID, nil)
		return domain.NewError(domain.ErrClassIntegrity, domain.DiskErrorRemovedDuringOperation,
			"the disk was disconnected during the operation, so ROTUP cannot confirm it was flushed safely")
	}

	s.setStatus(ctx, disk.ID, domain.DiskSafeToRemove, "")
	s.event(ctx, domain.SeverityInfo, "disk.unmounted", disk.ID,
		map[string]any{"mount_point": expected})
	return nil
}

// resolveForOperation finds the single filesystem carrying the registered UUID,
// refusing both absence and ambiguity.
func (s *Service) resolveForOperation(ctx context.Context, disk domain.Disk) (domain.ObservedMedia, error) {
	current, err := s.observe(ctx)
	if err != nil {
		return domain.ObservedMedia{}, err
	}
	matches := matchingMedia(current.Media, disk.FilesystemUUID)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return domain.ObservedMedia{}, domain.NewError(domain.ErrClassUnavailable, "disk_not_connected",
			"this disk is not currently connected")
	default:
		return domain.ObservedMedia{}, domain.NewError(domain.ErrClassIntegrity,
			domain.DiskErrorAmbiguousIdentity,
			"more than one connected filesystem claims this disk's UUID; ROTUP will not guess which one to use")
	}
}

func (s *Service) requireDisk(ctx context.Context, diskID string) (*domain.Disk, error) {
	disk, err := s.store.Disk(ctx, diskID)
	if err != nil {
		return nil, err
	}
	if disk == nil {
		return nil, domain.NewError(domain.ErrClassConfiguration, "disk_missing",
			"that disk is not registered")
	}
	return disk, nil
}

// failOperation records a typed failure that happened before anything was
// mounted, mapping absence back to DISCONNECTED rather than to an error the
// administrator has to clear.
func (s *Service) failOperation(ctx context.Context, diskID string, err error) {
	var typed *domain.Error
	code := domain.DiskErrorMountFailed
	if domain.AsError(err, &typed) {
		switch typed.Code {
		case "disk_not_connected":
			s.setStatus(ctx, diskID, domain.DiskDisconnected, "")
			s.event(ctx, domain.SeverityWarning, "disk.mount_failed", diskID,
				map[string]any{"error_code": "disk_not_connected"})
			return
		case domain.DiskErrorAmbiguousIdentity:
			code = domain.DiskErrorAmbiguousIdentity
		}
	}
	s.setStatus(ctx, diskID, domain.DiskError, code)
	s.event(ctx, domain.SeverityError, "disk.mount_failed", diskID,
		map[string]any{"error_code": code, "reason": safeMessage(err)})
}

func (s *Service) setStatus(ctx context.Context, diskID string, status domain.DiskStatus, errorCode string) {
	state := domain.DiskState{ID: diskID, Status: status, ErrorCode: errorCode}
	now := s.now()
	switch status {
	case domain.DiskDisconnected:
		state.LastDisconnectedAt = &now
	}
	if err := s.store.SaveDiskState(ctx, state); err != nil {
		s.logger.Error("disk state could not be persisted", "disk_id", diskID, "error", err)
	}
}

func supportedList() string {
	list := ""
	for i, name := range domain.SupportedFilesystems {
		if i > 0 {
			list += ", "
		}
		list += name
	}
	return list
}
