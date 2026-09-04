package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/system"
)

// Onboarding truth is derived here, in Go, from persisted state. The browser
// renders what this endpoint says and never recomputes it: the distinctions
// that matter are exactly the ones a frontend gets wrong. A configured source
// is not a reachable one, an existing repository is not an established one, a
// registered disk is not a connected one, and a published local copy is not a
// backup.

// setupStepID names one onboarding step. The values are stable so the frontend
// and any support script can match on them.
type setupStepID string

const (
	stepSource        setupStepID = "SOURCE_CONFIGURED"
	stepCredential    setupStepID = "SOURCE_CREDENTIAL_SAVED"
	stepConnection    setupStepID = "SOURCE_CONNECTION_TESTED"
	stepCollection    setupStepID = "FIRST_COLLECTION_COMPLETED"
	stepDisk          setupStepID = "BACKUP_DISK_REGISTERED"
	stepStorage       setupStepID = "BACKUP_STORAGE_PREPARED"
	stepDiskConnected setupStepID = "BACKUP_DISK_CONNECTED"
	stepBackup        setupStepID = "FIRST_BACKUP_COMPLETED"
)

type setupStepDTO struct {
	ID setupStepID `json:"id"`
	// Title is what the step is, in operator language.
	Title string `json:"title"`
	// Done is true only when persisted state proves the step happened.
	Done bool `json:"done"`
	// Evidence states what ROTUP actually observed, including the reason a
	// step is not done. It never claims more than the record supports.
	Evidence string `json:"evidence"`
	// Action names the operation that completes the step, and Href is where it
	// is performed.
	Action string `json:"action"`
	Href   string `json:"href"`
	// Blocked marks a step that cannot be attempted yet because the system
	// itself is not provisioned, rather than because the operator has not got
	// to it. The browser must not offer sudo; it points at the CLI instead.
	Blocked bool `json:"blocked"`
}

type setupSystemDTO struct {
	// Ready is false when the host's own integration has an error. It is
	// distinct from application readiness on purpose: one is fixed with
	// `rotupd setup`, the other in this UI.
	Ready bool `json:"ready"`
	// Checked is false when the daemon could not inspect the host at all.
	Checked bool `json:"checked"`
	// Issues are the system findings an operator has to act on.
	Issues []setupIssueDTO `json:"issues"`
	// Command is what to run to fix them.
	Command string `json:"command"`
	// MediaAvailable reports whether removable-media management is wired up at
	// all. Without it the disk and storage steps cannot be attempted.
	MediaAvailable bool `json:"mediaAvailable"`
}

type setupIssueDTO struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
	Critical bool   `json:"critical"`
}

type setupDTO struct {
	System      setupSystemDTO `json:"system"`
	Steps       []setupStepDTO `json:"steps"`
	Complete    bool           `json:"complete"`
	NextStep    *setupStepDTO  `json:"nextStep"`
	GeneratedAt time.Time      `json:"generatedAt"`
	// RecentRunLimit repeats the history bound the evidence was drawn from.
	// An unconfirmed step within it is not proof the step never happened.
	RecentRunLimit int `json:"recentRunLimit"`
}

func (h *handler) apiSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	dashboard, err := h.dashboardInfo(ctx)
	if err != nil {
		h.apiError(w, err)
		return
	}
	h.apiJSON(w, 200, h.setupState(ctx, dashboard))
}

// systemReadiness runs the same read-only inspection the doctor command uses.
// The daemon is unprivileged, so several checks can only report "cannot verify
// from here"; that is reported as it is rather than assumed to be fine.
func (h *handler) systemReadiness(ctx context.Context) setupSystemDTO {
	state := setupSystemDTO{Checked: true, Command: "sudo rotupd setup", Issues: []setupIssueDTO{}}
	if h.deps.Media != nil {
		state.MediaAvailable = h.deps.Media.PlatformAvailable()
	}
	if h.deps.SystemInspect == nil {
		state.Checked = false
		state.Ready = true
		return state
	}
	report := h.deps.SystemInspect(ctx)
	state.Ready = report.Ready()
	for _, check := range report.Failures() {
		state.Issues = append(state.Issues, setupIssueDTO{
			ID: check.ID, Title: check.Title, Status: string(check.Status),
			Detail: check.Detail, Remedy: check.Remedy,
			Critical: check.Status == system.StatusError,
		})
	}
	return state
}

// setupState derives every step from the dashboard read model, which is itself
// built from persisted state.
func (h *handler) setupState(ctx context.Context, data dashboardDTO) setupDTO {
	systemState := h.systemReadiness(ctx)
	state := setupDTO{
		System:         systemState,
		GeneratedAt:    time.Now().UTC(),
		RecentRunLimit: data.RecentRunLimit,
		Steps:          []setupStepDTO{},
	}

	source := firstSource(data.Sources)
	sourceHref := "/sources/new"
	if source != nil {
		sourceHref = "/sources/" + source.ID
	}

	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepSource, Title: "Source configured",
		Done:     source != nil,
		Evidence: sourceEvidence(data.Sources),
		Action:   "Add a source", Href: sourceHref,
	})

	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepCredential, Title: "Share password saved",
		Done:     source != nil && source.HasCredential,
		Evidence: credentialEvidence(source),
		Action:   "Save the password", Href: sourceHref,
	})

	// A source that exists is not one ROTUP has proved it can read. Only a
	// recorded successful connection test says that.
	test := latestRunOfKind(data.RecentRuns, string(domain.RunKindConnectionTest), true)
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepConnection, Title: "Connection tested successfully",
		Done:     test != nil,
		Evidence: runEvidence(test, "a successful connection test", data.RecentRunLimit),
		Action:   "Test the connection", Href: sourceHref,
	})

	// Published state, not a run that happened to finish: the collection is
	// only complete when the source records a published local copy.
	published := publishedSource(data.Sources)
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepCollection, Title: "First collection published",
		Done:     published != nil,
		Evidence: collectionEvidence(published),
		Action:   "Collect this source", Href: sourceHref,
	})

	registered := registeredDisks(data.Storage.Disks)
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepDisk, Title: "Backup disk registered",
		Done:     len(registered) > 0,
		Evidence: diskEvidence(registered, data.Storage),
		Action:   "Register a backup disk", Href: "/disks",
		Blocked: !systemState.MediaAvailable,
	})

	// An existing repository row is not prepared storage; only an established
	// one is, and the backend is what decides that.
	storage := establishedRepository(data.Repositories)
	storageHref := "/repositories"
	if storage != nil {
		storageHref = "/repositories/" + storage.ID
	} else if len(registered) > 0 {
		storageHref = "/repositories/new?disk=" + registered[0].ID
	}
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepStorage, Title: "Backup storage prepared",
		Done:     storage != nil,
		Evidence: storageEvidence(data.Repositories),
		Action:   "Prepare backup storage", Href: storageHref,
		Blocked: !systemState.MediaAvailable,
	})

	// A backup cannot start while its disk is away. This is a real precondition
	// and it is listed as one rather than letting the operator find a disabled
	// button at the end of the flow.
	connected := storage != nil && storage.Connected
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepDiskConnected, Title: "Backup disk connected",
		Done:     connected,
		Evidence: connectionEvidence(storage),
		Action:   "Connect the backup disk", Href: diskHref(storage, registered),
	})

	// The persisted durable-success timestamp, never a snapshot identifier and
	// never the presence of a backup run in a bounded window.
	state.Steps = append(state.Steps, setupStepDTO{
		ID: stepBackup, Title: "First backup completed",
		Done:     data.LastSuccessfulBackupAt != nil,
		Evidence: backupEvidence(data.LastSuccessfulBackupAt),
		Action:   "Back up now", Href: storageHref,
	})

	state.Complete = true
	for i := range state.Steps {
		if state.Steps[i].Done {
			continue
		}
		state.Complete = false
		if state.NextStep == nil {
			step := state.Steps[i]
			state.NextStep = &step
		}
	}
	return state
}

func firstSource(sources []sourceDTO) *sourceDTO {
	for i := range sources {
		return &sources[i]
	}
	return nil
}

func publishedSource(sources []sourceDTO) *sourceDTO {
	for i := range sources {
		if sources[i].LastPublishedAt != nil {
			return &sources[i]
		}
	}
	return nil
}

func registeredDisks(disks []diskDTO) []diskDTO {
	found := []diskDTO{}
	for _, disk := range disks {
		if disk.Registered {
			found = append(found, disk)
		}
	}
	return found
}

func establishedRepository(repositories []repositoryDTO) *repositoryDTO {
	for i := range repositories {
		if repositories[i].Established {
			return &repositories[i]
		}
	}
	return nil
}

func latestRunOfKind(runs []runDTO, kind string, successful bool) *runDTO {
	for i := range runs {
		if runs[i].Kind != kind {
			continue
		}
		// SUCCEEDED_WITH_WARNING is still a success; the domain owns that rule.
		if successful && !domain.RunStatus(runs[i].Status).Successful() {
			continue
		}
		return &runs[i]
	}
	return nil
}

func sourceEvidence(sources []sourceDTO) string {
	switch len(sources) {
	case 0:
		return "No source is configured."
	case 1:
		return "1 source configured: " + sources[0].Name + "."
	default:
		return "Several sources are configured."
	}
}

func credentialEvidence(source *sourceDTO) string {
	switch {
	case source == nil:
		return "Configure a source first."
	case source.HasCredential:
		return "A password is saved for " + source.Name + ". Its value is never displayed."
	default:
		return "No password is saved for " + source.Name + "."
	}
}

func runEvidence(run *runDTO, what string, limit int) string {
	if run == nil {
		return "There is no record of " + what + " in the latest " + strconv.Itoa(limit) +
			" runs. That is not proof one never happened."
	}
	if run.FinishedAt != nil {
		return "Succeeded " + run.FinishedAt.Format(time.RFC3339) + "."
	}
	return "Recorded as successful."
}

func collectionEvidence(source *sourceDTO) string {
	if source == nil {
		return "No source has published a local copy yet. A collection run that has not published is not a completed collection."
	}
	return "Local copy of " + source.Name + " published " + source.LastPublishedAt.Format(time.RFC3339) + "."
}

func diskEvidence(registered []diskDTO, storage disksDTO) string {
	if !storage.PlatformAvailable {
		return "Removable-media management is unavailable on this host. " + storage.PlatformReason
	}
	if storage.EnumerationError != "" {
		return "Disk discovery failed, so presence cannot be confirmed: " + storage.EnumerationError
	}
	if len(registered) == 0 {
		return "No disk is registered."
	}
	return strconv.Itoa(len(registered)) + " disk(s) registered, identified by filesystem UUID."
}

func storageEvidence(repositories []repositoryDTO) string {
	if len(repositories) == 0 {
		return "No backup storage is prepared."
	}
	for _, repository := range repositories {
		if repository.Established {
			return "Storage on " + repository.DiskName + " is established and reports " + repository.Health + "."
		}
	}
	// A row without an establishment timestamp means setup started and did not
	// finish. Saying "prepared" here would be exactly the false completion the
	// backend refuses to record.
	return "Preparation was started but has not completed. The repository identity is not established."
}

func connectionEvidence(storage *repositoryDTO) string {
	if storage == nil {
		return "Prepare backup storage first."
	}
	if storage.Connected {
		return storage.DiskName + " is connected now."
	}
	return storage.DiskName + " is not connected. Connect it before starting a backup."
}

func diskHref(storage *repositoryDTO, registered []diskDTO) string {
	if storage != nil && storage.DiskID != "" {
		return "/disks/" + storage.DiskID
	}
	if len(registered) > 0 {
		return "/disks/" + registered[0].ID
	}
	return "/disks"
}

func backupEvidence(at *time.Time) string {
	if at == nil {
		return "No completed backup to a disk is recorded. A published local copy stays on this machine and is not a backup."
	}
	return "Last completed backup " + at.Format(time.RFC3339) + "."
}
