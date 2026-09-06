package schedule

import (
	"strings"

	"github.com/bejusxd/rotup/internal/domain"
)

// keyVersion prefixes every generated key. It is part of the stored value
// because the keys live in a UNIQUE column: if the derivation ever changes, old
// and new keys must not be able to collide, and the version is what guarantees
// that.
const keyVersion = "rotup/v1"

// RunKey derives the idempotency key that prevents one nominal due time
// producing more than one run.
//
// The architecture defines the key as job, nominal due time and trigger kind.
// This derivation is exactly those three and nothing else, which has one
// consequence worth stating rather than discovering: the schedule is **not**
// part of the key. Two schedules on the same job that fall on the same nominal
// minute therefore deduplicate to a single run. That is the specified
// behaviour, not an accident of this encoding — a job is the unit of work, and
// running it twice at one instant because it was configured twice would be a
// defect, not a feature.
//
// Encoding is the other thing that has to be deliberate. Concatenating fields
// with a separator is ambiguous the moment a field can contain that separator:
// job "a|due=x" would otherwise collide with a different job at a different
// time. Every value is therefore percent-encoded down to an unreserved
// alphabet, so no value can contain a delimiter and no two distinct inputs can
// produce the same key.
func RunKey(jobID string, nominal Nominal, trigger domain.RunTrigger) (string, error) {
	if strings.TrimSpace(jobID) == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "idempotency_job_missing",
			"an idempotency key needs the job the run belongs to")
	}
	if nominal.Date == "" || nominal.Time == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "idempotency_due_missing",
			"an idempotency key needs a complete nominal due time")
	}
	// A non-canonical spelling would encode cleanly and produce a second key for
	// a due time that already has one, which is precisely the duplicate this key
	// exists to prevent. Refusing is the only safe answer; normalising here
	// would hide the caller's bug behind a key that no longer matches what it
	// stored.
	if err := nominal.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(string(trigger)) == "" {
		return "", domain.NewError(domain.ErrClassInvariant, "idempotency_trigger_missing",
			"an idempotency key needs the trigger kind")
	}
	return keyVersion +
		"|job=" + encodeKeyField(jobID) +
		"|due=" + encodeKeyField(nominal.String()) +
		"|trigger=" + encodeKeyField(string(trigger)), nil
}

// encodeKeyField percent-encodes everything outside a deliberately small
// unreserved set. Keeping letters, digits, '-', '.', '_' and '~' legible means a
// key can still be read in a database row or a log line while remaining
// unambiguous; ':' in a timestamp and '/' in a path-like identifier both encode,
// which is the point.
func encodeKeyField(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			builder.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			builder.WriteByte('%')
			builder.WriteByte(hex[c>>4])
			builder.WriteByte(hex[c&0x0f])
		}
	}
	return builder.String()
}
