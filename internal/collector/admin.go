package collector

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/ports"
	"github.com/bejusxd/rotup/internal/staging"
)

// AdminStore is the repository seam source administration needs.
type AdminStore interface {
	CreateSource(ctx context.Context, source domain.Source) error
	UpdateSourceSettings(ctx context.Context, source domain.Source) error
	Source(ctx context.Context, id string) (*domain.Source, error)
	ListSources(ctx context.Context) ([]domain.Source, error)
	StagingPaths(ctx context.Context) (map[string]string, error)
	CredentialRef(ctx context.Context, sourceID string) (domain.CredentialRef, error)
	AttachCredential(ctx context.Context, sourceID string, ref domain.CredentialRef) (string, error)
	ClearCredential(ctx context.Context, sourceID string) (string, error)
	AppendEvent(ctx context.Context, event domain.Event) error
}

// Admin implements the source configuration and credential use cases the web
// layer calls. It keeps validation and secret lifecycle policy out of handlers.
type Admin struct {
	store   AdminStore
	secrets ports.SecretStore
	layout  *staging.Layout
	logger  *slog.Logger
	now     func() time.Time
}

// NewAdmin builds the source administration use cases.
func NewAdmin(store AdminStore, secretStore ports.SecretStore, layout *staging.Layout, logger *slog.Logger) *Admin {
	if logger == nil {
		logger = slog.Default()
	}
	return &Admin{
		store:   store,
		secrets: secretStore,
		layout:  layout,
		logger:  logger.With("component", "sources"),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SourceInput is the administrator-supplied, non-secret part of a source.
// A password is never part of it: credentials are replaced through their own
// explicit action.
type SourceInput struct {
	Name                 string
	Host                 string
	ShareName            string
	Subdirectory         string
	Username             string
	StagingRelativePath  string
	Enabled              bool
	IncludeRules         []string
	ExcludeRules         []string
	ReconciliationPolicy domain.ReconciliationPolicy
}

// Create validates and stores a new source, returning its identifier.
func (a *Admin) Create(ctx context.Context, input SourceInput) (string, error) {
	id, err := domain.NewID()
	if err != nil {
		return "", err
	}
	source, err := a.buildSource(ctx, id, input)
	if err != nil {
		return "", err
	}
	source.StagingStatus = domain.StagingEmpty
	if err := a.store.CreateSource(ctx, source); err != nil {
		return "", err
	}
	// Creating the staging directories now means an invalid destination is
	// rejected at configuration time rather than at the first collection.
	if _, err := a.layout.CurrentDir(source); err != nil {
		return "", err
	}
	a.audit(ctx, domain.SeverityInfo, "source.created", id, map[string]any{"name": source.Name})
	return id, nil
}

// Update rewrites a source's non-secret settings.
func (a *Admin) Update(ctx context.Context, id string, input SourceInput) error {
	existing, err := a.store.Source(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return domain.NewError(domain.ErrClassConfiguration, "source_missing",
			"the source no longer exists")
	}
	source, err := a.buildSource(ctx, id, input)
	if err != nil {
		return err
	}
	if err := a.store.UpdateSourceSettings(ctx, source); err != nil {
		return err
	}
	a.audit(ctx, domain.SeverityInfo, "source.updated", id, map[string]any{"name": source.Name})
	return nil
}

func (a *Admin) buildSource(ctx context.Context, id string, input SourceInput) (domain.Source, error) {
	existing, err := a.store.StagingPaths(ctx)
	if err != nil {
		return domain.Source{}, err
	}
	source := domain.Source{
		ID:                   id,
		Name:                 input.Name,
		Host:                 input.Host,
		ShareName:            input.ShareName,
		Subdirectory:         input.Subdirectory,
		Username:             input.Username,
		StagingRelativePath:  input.StagingRelativePath,
		Enabled:              input.Enabled,
		IncludeRules:         input.IncludeRules,
		ExcludeRules:         input.ExcludeRules,
		ReconciliationPolicy: input.ReconciliationPolicy,
	}
	if err := domain.ValidateSource(&source, existing); err != nil {
		return domain.Source{}, err
	}
	return source, nil
}

// ReplaceCredential stores a new password for a source and detaches the one it
// replaced. The value is written to the secret provider, never to SQLite, and
// is never returned to a caller afterwards.
func (a *Admin) ReplaceCredential(ctx context.Context, sourceID, password string) error {
	if strings.TrimSpace(password) == "" {
		return domain.NewError(domain.ErrClassConfiguration, "credential_empty",
			"the password must not be empty; use the clear action to remove a stored credential")
	}
	source, err := a.store.Source(ctx, sourceID)
	if err != nil {
		return err
	}
	if source == nil {
		return domain.NewError(domain.ErrClassConfiguration, "source_missing",
			"the source no longer exists")
	}

	locator, err := a.secrets.Put(ctx, domain.SecretPurposeSMBPassword, password)
	if err != nil {
		return err
	}
	refID, err := domain.NewID()
	if err != nil {
		_ = a.secrets.Delete(ctx, locator)
		return err
	}
	ref := domain.CredentialRef{
		ID:              refID,
		DisplayName:     "SMB password for " + source.Name,
		Provider:        a.secrets.Provider(),
		ProviderLocator: locator,
		Purpose:         domain.SecretPurposeSMBPassword,
	}
	displaced, err := a.store.AttachCredential(ctx, sourceID, ref)
	if err != nil {
		// The metadata row did not commit, so the material it would have
		// addressed must not be left behind.
		_ = a.secrets.Delete(ctx, locator)
		return err
	}
	if displaced != "" {
		if err := a.secrets.Delete(ctx, displaced); err != nil {
			a.logger.Error("displaced credential material could not be removed", "source_id", sourceID, "error", err)
		}
	}
	a.audit(ctx, domain.SeverityInfo, "credential.replaced", sourceID,
		map[string]any{"purpose": string(domain.SecretPurposeSMBPassword), "credential_ref_id": refID})
	return nil
}

// ClearCredential removes a source's stored password and its metadata row.
func (a *Admin) ClearCredential(ctx context.Context, sourceID string) error {
	locator, err := a.store.ClearCredential(ctx, sourceID)
	if err != nil {
		return err
	}
	if locator == "" {
		return nil
	}
	if err := a.secrets.Delete(ctx, locator); err != nil {
		return err
	}
	a.audit(ctx, domain.SeverityInfo, "credential.cleared", sourceID,
		map[string]any{"purpose": string(domain.SecretPurposeSMBPassword)})
	return nil
}

func (a *Admin) audit(ctx context.Context, severity, eventType, sourceID string, payload map[string]any) {
	event := domain.Event{
		OccurredAt:  a.now(),
		Severity:    severity,
		EventType:   eventType,
		Component:   "sources",
		SourceID:    sourceID,
		PayloadJSON: encodeSummary(payload),
	}
	if err := a.store.AppendEvent(ctx, event); err != nil {
		a.logger.Error("audit event could not be persisted", "event_type", eventType, "error", err)
	}
}
