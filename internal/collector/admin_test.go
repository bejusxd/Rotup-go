package collector

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

// fakeSecretStore records what the credential use cases ask of the provider.
type fakeSecretStore struct {
	mu      sync.Mutex
	values  map[string]string
	counter int
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{values: make(map[string]string)}
}

func (f *fakeSecretStore) Provider() string { return "fake" }

func (f *fakeSecretStore) Put(_ context.Context, _ domain.SecretPurpose, value string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	locator := "locator-" + string(rune('a'+f.counter-1))
	f.values[locator] = value
	return locator, nil
}

func (f *fakeSecretStore) Materialize(_ context.Context, locator string, fn func(domain.SecretMaterial) error) error {
	f.mu.Lock()
	value, ok := f.values[locator]
	f.mu.Unlock()
	if !ok {
		return domain.NewError(domain.ErrClassConfiguration, "secret_missing", "no such secret")
	}
	return fn(domain.NewSecretMaterial(value))
}

func (f *fakeSecretStore) Delete(_ context.Context, locator string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, locator)
	return nil
}

func (f *fakeSecretStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.values)
}

func TestCredentialIsStoredOutsideOrdinarySourceRows(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	const password = "correct horse battery staple"

	if err := h.admin.ReplaceCredential(context.Background(), h.source.ID, password); err != nil {
		t.Fatalf("store credential: %v", err)
	}

	source, err := h.store.Source(context.Background(), h.source.ID)
	if err != nil || source == nil {
		t.Fatalf("load source: %v", err)
	}
	if !source.HasCredential() {
		t.Fatal("the source should reference a credential")
	}

	// Nothing anywhere in the source row, its metadata, or the credential
	// reference may contain the secret value.
	if strings.Contains(sourceFingerprint(*source), password) {
		t.Fatal("the source row must not contain the password")
	}
	ref, err := h.store.CredentialRef(context.Background(), h.source.ID)
	if err != nil {
		t.Fatalf("load credential reference: %v", err)
	}
	fingerprint := ref.ID + ref.DisplayName + ref.Provider + ref.ProviderLocator + string(ref.Purpose)
	if strings.Contains(fingerprint, password) {
		t.Fatal("the credential reference metadata must not contain the password")
	}
	if ref.ProviderLocator == "" {
		t.Fatal("the credential reference must address provider material")
	}

	// The material itself is retrievable only through the provider callback.
	var revealed string
	if err := h.secrets.Materialize(context.Background(), ref.ProviderLocator, func(material domain.SecretMaterial) error {
		revealed = material.Reveal()
		return nil
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if revealed != password {
		t.Fatalf("materialized value = %q, want the stored password", revealed)
	}
}

func TestReplacingCredentialRemovesTheDisplacedMaterial(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	if err := h.admin.ReplaceCredential(context.Background(), h.source.ID, "first-password"); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	firstRef, err := h.store.CredentialRef(context.Background(), h.source.ID)
	if err != nil {
		t.Fatalf("load credential reference: %v", err)
	}

	if err := h.admin.ReplaceCredential(context.Background(), h.source.ID, "second-password"); err != nil {
		t.Fatalf("replace credential: %v", err)
	}
	secondRef, err := h.store.CredentialRef(context.Background(), h.source.ID)
	if err != nil {
		t.Fatalf("load credential reference: %v", err)
	}

	if secondRef.ProviderLocator == firstRef.ProviderLocator {
		t.Fatal("replacing a credential must store new provider material")
	}
	if h.secrets.count() != 1 {
		t.Fatalf("provider holds %d secrets, want exactly the current one", h.secrets.count())
	}
	var revealed string
	if err := h.secrets.Materialize(context.Background(), secondRef.ProviderLocator, func(m domain.SecretMaterial) error {
		revealed = m.Reveal()
		return nil
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if revealed != "second-password" {
		t.Fatalf("materialized value = %q, want the replacement password", revealed)
	}
}

func TestClearingCredentialRemovesReferenceAndMaterial(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	if err := h.admin.ReplaceCredential(context.Background(), h.source.ID, "a-password"); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	if err := h.admin.ClearCredential(context.Background(), h.source.ID); err != nil {
		t.Fatalf("clear credential: %v", err)
	}

	source, err := h.store.Source(context.Background(), h.source.ID)
	if err != nil || source == nil {
		t.Fatalf("load source: %v", err)
	}
	if source.HasCredential() {
		t.Fatal("the source must no longer reference a credential")
	}
	if h.secrets.count() != 0 {
		t.Fatalf("provider still holds %d secrets after clearing", h.secrets.count())
	}
	// Clearing again is harmless.
	if err := h.admin.ClearCredential(context.Background(), h.source.ID); err != nil {
		t.Fatalf("second clear: %v", err)
	}
}

func TestReplacingCredentialRejectsAnEmptyValue(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	if err := h.admin.ReplaceCredential(context.Background(), h.source.ID, "   "); err == nil {
		t.Fatal("an empty password must be rejected rather than silently stored")
	}
	if h.secrets.count() != 0 {
		t.Fatal("a rejected password must not leave provider material behind")
	}
}

func TestOverlappingStagingDestinationsAreRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t, domain.PolicyArchiveThenSync)
	cases := map[string]string{
		"identical destination": "documents",
		"nested below existing": "documents/reports",
		"parent of existing":    ".",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.admin.Create(context.Background(), SourceInput{
				Name:                 "another-" + name,
				Host:                 "fileserver.example",
				ShareName:            "docs",
				StagingRelativePath:  path,
				Enabled:              true,
				ReconciliationPolicy: domain.PolicyArchiveThenSync,
			})
			if err == nil {
				t.Fatalf("staging destination %q should have been rejected", path)
			}
		})
	}
}

func sourceFingerprint(source domain.Source) string {
	return strings.Join([]string{
		source.ID, source.Name, source.Host, source.ShareName, source.Subdirectory,
		source.Username, source.CredentialRefID, source.StagingRelativePath,
		strings.Join(source.IncludeRules, ","), strings.Join(source.ExcludeRules, ","),
		string(source.ReconciliationPolicy), string(source.StagingStatus),
	}, "|")
}
