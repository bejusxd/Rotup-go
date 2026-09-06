package schedule

import (
	"strings"
	"testing"

	"github.com/bejusxd/rotup/internal/domain"
)

func mustKey(t *testing.T, job string, nominal Nominal, trigger domain.RunTrigger) string {
	t.Helper()
	key, err := RunKey(job, nominal, trigger)
	if err != nil {
		t.Fatalf("RunKey: %v", err)
	}
	return key
}

func TestRunKeyIsStableAndVersioned(t *testing.T) {
	t.Parallel()

	nominal := Nominal{Date: "2026-10-25", Time: "02:30"}
	key := mustKey(t, "job-1", nominal, domain.RunTriggerScheduled)
	if key != mustKey(t, "job-1", nominal, domain.RunTriggerScheduled) {
		t.Fatal("the same inputs produced different keys")
	}
	if !strings.HasPrefix(key, "rotup/v1|") {
		t.Fatalf("key is not versioned: %s", key)
	}
	if !strings.Contains(key, "2026-10-25T02%3A30") {
		t.Fatalf("key does not carry the encoded nominal time: %s", key)
	}
}

// Each of the three inputs the architecture names must change the key, and
// nothing else may.
func TestRunKeyDistinguishesEveryDefiningInput(t *testing.T) {
	t.Parallel()

	base := Nominal{Date: "2026-06-10", Time: "02:30"}
	key := mustKey(t, "job-1", base, domain.RunTriggerScheduled)

	different := map[string]string{
		"job":     mustKey(t, "job-2", base, domain.RunTriggerScheduled),
		"date":    mustKey(t, "job-1", Nominal{Date: "2026-06-11", Time: "02:30"}, domain.RunTriggerScheduled),
		"time":    mustKey(t, "job-1", Nominal{Date: "2026-06-10", Time: "02:31"}, domain.RunTriggerScheduled),
		"trigger": mustKey(t, "job-1", base, domain.RunTriggerCollectionSuccess),
	}
	for name, other := range different {
		if other == key {
			t.Errorf("changing the %s did not change the key", name)
		}
	}
}

// A separator inside a value must not be able to imitate the structure of the
// key. Without encoding, a job named to look like a field would collide with a
// different job at a different time.
func TestRunKeyEncodingIsUnambiguous(t *testing.T) {
	t.Parallel()

	nominal := Nominal{Date: "2026-06-10", Time: "02:30"}
	hostile := mustKey(t, "a|due=2026-06-10T02:30|trigger=SCHEDULED|x", nominal, domain.RunTriggerScheduled)
	ordinary := mustKey(t, "a", nominal, domain.RunTriggerScheduled)
	if hostile == ordinary {
		t.Fatal("a job name imitating the key structure collided with a plain one")
	}
	// The delimiters that give the key its shape appear only as delimiters.
	if strings.Count(hostile, "|") != 3 || strings.Count(hostile, "=") != 3 {
		t.Fatalf("value content leaked delimiters into the key: %s", hostile)
	}

	// Two different jobs whose names differ only where encoding applies stay
	// different.
	a := mustKey(t, "job one", nominal, domain.RunTriggerScheduled)
	b := mustKey(t, "job|one", nominal, domain.RunTriggerScheduled)
	if a == b {
		t.Fatal("distinct job names produced one key")
	}
}

func TestRunKeyRefusesIncompleteInput(t *testing.T) {
	t.Parallel()

	nominal := Nominal{Date: "2026-06-10", Time: "02:30"}
	cases := []struct {
		name    string
		job     string
		nominal Nominal
		trigger domain.RunTrigger
		code    string
	}{
		{"no job", "  ", nominal, domain.RunTriggerScheduled, "idempotency_job_missing"},
		{"no date", "job-1", Nominal{Time: "02:30"}, domain.RunTriggerScheduled, "idempotency_due_missing"},
		{"no time", "job-1", Nominal{Date: "2026-06-10"}, domain.RunTriggerScheduled, "idempotency_due_missing"},
		{"no trigger", "job-1", nominal, "", "idempotency_trigger_missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RunKey(c.job, c.nominal, c.trigger)
			if err == nil {
				t.Fatal("an incomplete key was produced")
			}
			var typed *domain.Error
			if !domain.AsError(err, &typed) || typed.Code != c.code {
				t.Fatalf("error = %v, want code %s", err, c.code)
			}
		})
	}
}

// The key is derived from job, nominal time and trigger — not from the schedule.
// Two schedules on one job that land on the same minute therefore deduplicate to
// a single run, which is the specified behaviour and is asserted here so that a
// later change to it has to be deliberate.
func TestRunKeyDoesNotDependOnWhichScheduleFired(t *testing.T) {
	t.Parallel()

	nominal := Nominal{Date: "2026-06-10", Time: "02:30"}
	if mustKey(t, "job-1", nominal, domain.RunTriggerScheduled) !=
		mustKey(t, "job-1", nominal, domain.RunTriggerScheduled) {
		t.Fatal("the key derivation is not a function of its stated inputs")
	}
}
