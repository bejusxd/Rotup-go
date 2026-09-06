package schedule

import (
	"testing"
	"time"

	"github.com/bejusxd/rotup/internal/domain"
)

const warsaw = "Europe/Warsaw"

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone %s is unavailable on this host: %v", name, err)
	}
	return location
}

// at builds an instant from a local wall clock in a named zone. It refuses to
// build one whose wall clock the zone rewrote, so a test can never silently
// assert about a different moment than it wrote down.
func at(t *testing.T, zone, layout, value string) time.Time {
	t.Helper()
	location := mustLoad(t, zone)
	instant, err := time.ParseInLocation(layout, value, location)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	if instant.Format(layout) != value {
		t.Fatalf("%q does not exist in %s; it resolved to %s", value, zone, instant.Format(layout))
	}
	return instant
}

func everyDay() Spec {
	return Spec{Days: AllWeek, LocalTime: "02:30", Timezone: warsaw, Enabled: true}
}

func next(t *testing.T, s Spec, after time.Time) Occurrence {
	t.Helper()
	occurrence, ok, err := s.NextAfter(after)
	if err != nil {
		t.Fatalf("NextAfter: %v", err)
	}
	if !ok {
		t.Fatal("NextAfter reported no occurrence for an enabled schedule")
	}
	return occurrence
}

func TestNextAfterIsStrictlyAfterTheGivenInstant(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)
	s := everyDay()

	// Before, exactly at, and after the same due time.
	before := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-06-10 02:29"))
	if got := before.At.Format("2006-01-02 15:04"); got != "2026-06-10 02:30" {
		t.Fatalf("from just before, next = %s", got)
	}
	exact := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-06-10 02:30"))
	if got := exact.At.Format("2006-01-02 15:04"); got != "2026-06-11 02:30" {
		t.Fatalf("from exactly the due time, next = %s (it must not repeat)", got)
	}
	after := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-06-10 02:31"))
	if got := after.At.Format("2006-01-02 15:04"); got != "2026-06-11 02:30" {
		t.Fatalf("from just after, next = %s", got)
	}
}

func TestNextAfterSelectsOnlyChosenWeekdaysAndWrapsTheWeek(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)

	// 2026-06-10 is a Wednesday.
	weekdays := Spec{Days: Monday | Wednesday | Friday, LocalTime: "02:30",
		Timezone: warsaw, Enabled: true}
	from := at(t, warsaw, "2006-01-02 15:04", "2026-06-10 03:00") // Wednesday, past due
	if got := next(t, weekdays, from).At.Format("2006-01-02 15:04"); got != "2026-06-12 02:30" {
		t.Fatalf("next after Wednesday = %s, want Friday", got)
	}

	// A single selected day, asked on that day after the time has passed, wraps
	// a full week rather than firing again today or drifting to another day.
	onlyWednesday := Spec{Days: Wednesday, LocalTime: "02:30", Timezone: warsaw, Enabled: true}
	occurrence := next(t, onlyWednesday, from)
	if got := occurrence.At.Format("2006-01-02 15:04"); got != "2026-06-17 02:30" {
		t.Fatalf("single-day wrap = %s, want the following Wednesday", got)
	}
	if occurrence.At.Weekday() != time.Wednesday {
		t.Fatalf("wrapped onto %s", occurrence.At.Weekday())
	}

	// Sunday and Saturday are the two bits most easily transposed.
	weekend := Spec{Days: Saturday | Sunday, LocalTime: "02:30", Timezone: warsaw, Enabled: true}
	if got := next(t, weekend, from).At.Weekday(); got != time.Saturday {
		t.Fatalf("weekend schedule fired on %s", got)
	}
}

// When the clocks go forward the configured local time can simply not exist.
// Skipping the day would silently lose a backup, so the schedule fires at the
// first instant that has reached it.
func TestSpringForwardFiresAtTheFirstValidInstant(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)

	// Europe/Warsaw 2026-03-29: 02:00 CET becomes 03:00 CEST, so 02:30 is a gap.
	s := everyDay()
	from := at(t, warsaw, "2006-01-02 15:04", "2026-03-28 12:00")
	occurrence := next(t, s, from)

	if got := occurrence.At.Format("2006-01-02 15:04 MST"); got != "2026-03-29 03:00 CEST" {
		t.Fatalf("spring-forward occurrence = %s, want 03:00 CEST", got)
	}
	// The nominal identity is still what the administrator configured.
	if occurrence.Nominal.String() != "2026-03-29T02:30" {
		t.Fatalf("nominal = %s, want the configured local time", occurrence.Nominal)
	}
	// A time outside the gap on the same day is untouched.
	late := Spec{Days: AllWeek, LocalTime: "04:00", Timezone: warsaw, Enabled: true}
	if got := next(t, late, from).At.Format("2006-01-02 15:04 MST"); got != "2026-03-29 04:00 CEST" {
		t.Fatalf("non-gap time on a transition day = %s", got)
	}
}

// When the clocks go back the configured local time happens twice. It must fire
// once, on the first pass, and the second pass must not produce anything.
func TestFallBackFiresOnlyOnTheFirstOccurrence(t *testing.T) {
	t.Parallel()
	location := mustLoad(t, warsaw)

	// Europe/Warsaw 2026-10-25: 03:00 CEST becomes 02:00 CET, so 02:30 happens
	// twice — once at 00:30 UTC and again at 01:30 UTC.
	s := everyDay()
	firstPass := time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC).In(location)
	secondPass := time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC).In(location)
	if firstPass.Format("15:04") != "02:30" || secondPass.Format("15:04") != "02:30" {
		t.Skipf("this tzdata does not model the 2026 Warsaw fall-back as expected: %s / %s",
			firstPass.Format("15:04 MST"), secondPass.Format("15:04 MST"))
	}

	occurrence := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-10-24 12:00"))
	if !occurrence.At.Equal(firstPass) {
		t.Fatalf("fall-back occurrence = %s, want the first pass %s",
			occurrence.At.Format(time.RFC3339), firstPass.Format(time.RFC3339))
	}
	if occurrence.At.Format("MST") != "CEST" {
		t.Fatalf("fired in %s, want the summer-time pass", occurrence.At.Format("MST"))
	}
	if occurrence.Nominal.String() != "2026-10-25T02:30" {
		t.Fatalf("nominal = %s", occurrence.Nominal)
	}

	// Asked from between the two passes, the schedule must move on to the next
	// day rather than fire again on the repeated hour.
	between := firstPass.Add(15 * time.Minute)
	after := next(t, s, between)
	if after.At.Before(secondPass) || after.Nominal.Date == "2026-10-25" {
		t.Fatalf("the repeated hour produced a second occurrence: %s (%s)",
			after.At.Format(time.RFC3339), after.Nominal)
	}
	if got := after.At.Format("2006-01-02 15:04"); got != "2026-10-26 02:30" {
		t.Fatalf("after the repeated hour, next = %s", got)
	}

	// Asked from exactly the second pass, likewise.
	if got := next(t, s, secondPass).At.Format("2006-01-02 15:04"); got != "2026-10-26 02:30" {
		t.Fatalf("from the second pass, next = %s", got)
	}
}

// Both passes of a repeated local time share one nominal identity, so they
// cannot become two runs however the caller stumbles into them.
func TestRepeatedLocalTimeHasOneIdempotencyKey(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)
	s := everyDay()

	first := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-10-24 12:00"))
	// A caller that recomputed from a moment earlier in the same day gets the
	// same nominal value, and therefore the same key.
	same := next(t, s, at(t, warsaw, "2006-01-02 15:04", "2026-10-25 00:10"))

	firstKey, err := RunKey("job-1", first.Nominal, domain.RunTriggerScheduled)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	sameKey, err := RunKey("job-1", same.Nominal, domain.RunTriggerScheduled)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if firstKey != sameKey {
		t.Fatalf("the repeated hour produced two keys:\n%s\n%s", firstKey, sameKey)
	}
}

// Not every zone shifts by a whole hour. A thirty-minute shift must behave
// identically, which it only does if nothing assumes the size of the jump.
func TestNonHourDaylightShiftIsHandled(t *testing.T) {
	t.Parallel()
	const lordHowe = "Australia/Lord_Howe"
	location := mustLoad(t, lordHowe)

	// Lord Howe moves forward by thirty minutes: 02:00 becomes 02:30 on the
	// first Sunday in October. 02:15 therefore does not exist that day.
	transition := time.Date(2026, time.October, 4, 2, 15, 0, 0, location)
	if transition.Format("15:04") == "02:15" {
		t.Skipf("this tzdata does not model the Lord Howe transition as expected")
	}

	s := Spec{Days: AllWeek, LocalTime: "02:15", Timezone: lordHowe, Enabled: true}
	from := time.Date(2026, time.October, 3, 12, 0, 0, 0, location)
	occurrence := next(t, s, from)

	if got := occurrence.At.Format("2006-01-02 15:04"); got != "2026-10-04 02:30" {
		t.Fatalf("non-hour spring-forward = %s, want the first valid instant 02:30", got)
	}
	if occurrence.Nominal.String() != "2026-10-04T02:15" {
		t.Fatalf("nominal = %s, want the configured local time", occurrence.Nominal)
	}
}

func TestDisabledScheduleHasNoOccurrences(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)

	s := everyDay()
	s.Enabled = false
	occurrence, ok, err := s.NextAfter(at(t, warsaw, "2006-01-02 15:04", "2026-06-10 00:00"))
	if err != nil {
		t.Fatalf("a disabled schedule must not be an error: %v", err)
	}
	if ok {
		t.Fatalf("a disabled schedule produced %s", occurrence.At)
	}
	// It is still validated, so an unsavable schedule cannot hide behind being
	// switched off.
	if err := s.Validate(); err != nil {
		t.Fatalf("a valid disabled schedule failed validation: %v", err)
	}
}

func TestInvalidSpecsAreRejected(t *testing.T) {
	t.Parallel()

	valid := everyDay()
	cases := []struct {
		name string
		spec Spec
		code string
	}{
		{"no weekday", Spec{Days: 0, LocalTime: "02:30", Timezone: warsaw, Enabled: true},
			"schedule_no_weekday"},
		{"weekday out of range", Spec{Days: AllWeek + 1, LocalTime: "02:30", Timezone: warsaw, Enabled: true},
			"schedule_weekday_invalid"},
		{"unknown timezone", Spec{Days: AllWeek, LocalTime: "02:30", Timezone: "Mars/Olympus", Enabled: true},
			"schedule_timezone_unknown"},
		{"empty timezone", Spec{Days: AllWeek, LocalTime: "02:30", Timezone: "  ", Enabled: true},
			"schedule_timezone_missing"},
		{"host-local timezone", Spec{Days: AllWeek, LocalTime: "02:30", Timezone: "Local", Enabled: true},
			"schedule_timezone_ambiguous"},
		{"hour out of range", withTime(valid, "24:00"), "schedule_time_invalid"},
		{"minute out of range", withTime(valid, "02:60"), "schedule_time_invalid"},
		{"single digit hour", withTime(valid, "2:30"), "schedule_time_invalid"},
		{"seconds included", withTime(valid, "02:30:00"), "schedule_time_invalid"},
		{"padded", withTime(valid, " 02:30"), "schedule_time_invalid"},
		{"empty time", withTime(valid, ""), "schedule_time_invalid"},
		{"not a time", withTime(valid, "ab:cd"), "schedule_time_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if err == nil {
				t.Fatal("an invalid schedule was accepted")
			}
			var typed *domain.Error
			if !domain.AsError(err, &typed) || typed.Code != c.code {
				t.Fatalf("error = %v, want code %s", err, c.code)
			}
			if _, _, nextErr := c.spec.NextAfter(time.Now()); nextErr == nil {
				t.Fatal("an invalid schedule produced an occurrence")
			}
		})
	}
}

func withTime(s Spec, localTime string) Spec {
	s.LocalTime = localTime
	return s
}

// Midnight and the last minute of the day are the boundaries a day-stepping
// implementation is most likely to get wrong.
func TestDayBoundaryTimes(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)

	midnight := Spec{Days: AllWeek, LocalTime: "00:00", Timezone: warsaw, Enabled: true}
	got := next(t, midnight, at(t, warsaw, "2006-01-02 15:04", "2026-06-10 23:59"))
	if formatted := got.At.Format("2006-01-02 15:04"); formatted != "2026-06-11 00:00" {
		t.Fatalf("midnight schedule from 23:59 = %s", formatted)
	}
	if got.Nominal.String() != "2026-06-11T00:00" {
		t.Fatalf("midnight nominal = %s", got.Nominal)
	}

	lastMinute := Spec{Days: AllWeek, LocalTime: "23:59", Timezone: warsaw, Enabled: true}
	got = next(t, lastMinute, at(t, warsaw, "2006-01-02 15:04", "2026-06-10 00:00"))
	if formatted := got.At.Format("2006-01-02 15:04"); formatted != "2026-06-10 23:59" {
		t.Fatalf("last-minute schedule = %s", formatted)
	}
}

// The instant a caller passes in may be in any zone. What matters is the moment,
// not how it is written down.
func TestNextAfterIgnoresTheCallersZone(t *testing.T) {
	t.Parallel()
	mustLoad(t, warsaw)
	s := everyDay()

	local := at(t, warsaw, "2006-01-02 15:04", "2026-06-10 02:29")
	fromLocal := next(t, s, local)
	fromUTC := next(t, s, local.UTC())
	if !fromLocal.At.Equal(fromUTC.At) || fromLocal.Nominal != fromUTC.Nominal {
		t.Fatalf("the caller's zone changed the answer: %v vs %v", fromLocal, fromUTC)
	}
	// The returned instant is expressed in the schedule's own zone, so callers
	// and logs see the local time an administrator configured.
	if zone, _ := fromLocal.At.Zone(); zone != "CEST" {
		t.Fatalf("occurrence zone = %s, want the schedule's own", zone)
	}
}
