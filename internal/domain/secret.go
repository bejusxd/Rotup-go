package domain

import "log/slog"

// SecretPurpose describes what a stored secret is used for. It is metadata and
// is safe to persist and display.
type SecretPurpose string

const (
	// SecretPurposeSMBPassword is the password used to read an SMB source.
	SecretPurposeSMBPassword SecretPurpose = "SMB_PASSWORD"
	// SecretPurposeRepositoryPassword is the password that encrypts a restic
	// repository on one registered disk. Unlike a source password it cannot be
	// reset: losing it makes every snapshot on that media unreadable, which is
	// why establishing a repository requires an explicit escrow acknowledgement.
	SecretPurposeRepositoryPassword SecretPurpose = "REPOSITORY_PASSWORD"
)

// SecretProviderFile is the v1 provider: a service-owned directory of files.
const SecretProviderFile = "file"

// CredentialRef is the non-secret metadata row that points at material held by
// a secret provider. It never carries the secret value itself.
type CredentialRef struct {
	ID              string
	DisplayName     string
	Provider        string
	ProviderLocator string
	Purpose         SecretPurpose
	CreatedAt       string
	UpdatedAt       string
	LastUsedAt      string
}

// SecretMaterial carries plaintext secret material for the duration of a single
// operation. Its String and LogValue methods redact the value so that an
// accidental fmt or slog call cannot disclose it; only Reveal returns plaintext.
type SecretMaterial struct {
	value string
}

// NewSecretMaterial wraps plaintext in a redacting container.
func NewSecretMaterial(value string) SecretMaterial {
	return SecretMaterial{value: value}
}

// Reveal returns the plaintext. Callers must not store or log the result.
func (m SecretMaterial) Reveal() string { return m.value }

// Empty reports whether there is no material.
func (m SecretMaterial) Empty() bool { return m.value == "" }

// String implements fmt.Stringer with a redacted value.
func (m SecretMaterial) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer with a redacted value so %#v is also safe.
func (m SecretMaterial) GoString() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer with a redacted value.
func (m SecretMaterial) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
