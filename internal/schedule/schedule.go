// Package schedule computes when a job is next due from selected weekdays, one
// local execution time and a timezone. It is pure arithmetic: it reads no
// database, starts nothing, and knows nothing about runs or jobs beyond the
// identifiers it is handed.
//
// The v1 contract is fixed by the architecture: weekday checkboxes, one local
// time, a timezone, and enabled/disabled. There are no intervals, no multiple
// daily windows, no jitter and no cron expressions, and this package
// deliberately offers no way to express them.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// The embedded timezone database is a fallback the standard library uses
	// only when the host has no zoneinfo of its own, so an up-to-date system
	// database still wins. Importing it means a schedule cannot silently become
	// unevaluatable because a container image shipped without tzdata.
	_ "time/tzdata"

	"github.com/bejusxd/rotup/internal/domain"
)

// Weekdays is a set of selected days, one bit per weekday.
//
// Bit N is time.Weekday(N), so Sunday is bit 0 and Saturday is bit 6. That
// matches the standard library rather than ISO-8601 on purpose: every
// comparison in this package is against a time.Weekday, and a numbering that
// needs converting at each use is a numbering that eventually gets converted
// wrongly. How a user interface orders the checkboxes is its own business.
type Weekdays uint8

const (
	Sunday Weekdays = 1 << iota
	Monday
	Tuesday
	Wednesday
	Thursday
	Friday
	Saturday
)

// AllWeek is every day selected; it is also the largest legal mask.
const AllWeek Weekdays = Sunday | Monday | Tuesday | Wednesday | Thursday | Friday | Saturday

// Contains reports whether a weekday is selected.
func (w Weekdays) Contains(day time.Weekday) bool {
	return w&(1<<uint(day)) != 0
}

// Spec is one stored schedule. It mirrors the durable columns and holds no
// derived state: `next_run_at` is a cache of what this package computes, never
// an input to it.
type Spec struct {
	// Days must select at least one weekday.
	Days Weekdays
	// LocalTime is "HH:MM" in 24-hour form.
	LocalTime string
	// Timezone is an IANA name such as "Europe/Warsaw".
	Timezone string
	// Enabled false means the schedule has no occurrences at all.
	Enabled bool
}

// Nominal is the local identity of one due time: the wall-clock date and time an
// administrator selected, independent of which instant it resolved to.
//
// This is the identity that matters for deduplication. When a daylight-saving
// change makes a local time occur twice, both instants share one nominal value,
// so the repeated hour cannot produce two runs.
type Nominal struct {
	// Date is the local calendar date, "2006-01-02".
	Date string
	// Time is the configured local time, "15:04".
	Time string
}

// String returns the canonical form used in idempotency keys and history.
func (n Nominal) String() string { return n.Date + "T" + n.Time }

// Occurrence is one resolved firing: the instant it happens, and the nominal
// local time it stands for.
type Occurrence struct {
	// At is the instant, expressed in the schedule's own location.
	At time.Time
	// Nominal is the local due identity this instant fulfils.
	Nominal Nominal
}

// Validate reports whether the stored fields describe a schedule that can be
// evaluated. A disabled schedule is still validated: a schedule that cannot be
// enabled without erroring is not a schedule an administrator should be able to
// save.
func (s Spec) Validate() error {
	_, _, _, err := s.resolve()
	return err
}

func (s Spec) resolve() (*time.Location, int, int, error) {
	if s.Days == 0 {
		return nil, 0, 0, domain.NewError(domain.ErrClassConfiguration, "schedule_no_weekday",
			"a schedule must select at least one weekday")
	}
	if s.Days > AllWeek {
		return nil, 0, 0, domain.NewError(domain.ErrClassConfiguration, "schedule_weekday_invalid",
			"a schedule's weekday selection contains a day that does not exist")
	}
	hour, minute, err := parseLocalTime(s.LocalTime)
	if err != nil {
		return nil, 0, 0, err
	}
	if strings.TrimSpace(s.Timezone) == "" {
		return nil, 0, 0, domain.NewError(domain.ErrClassConfiguration, "schedule_timezone_missing",
			"a schedule must name a timezone")
	}
	// Local and UTC are accepted names, but "Local" would make the stored
	// schedule mean different things on different hosts, which is exactly what
	// naming a zone is for.
	if s.Timezone == "Local" {
		return nil, 0, 0, domain.NewError(domain.ErrClassConfiguration, "schedule_timezone_ambiguous",
			"a schedule must name a specific timezone rather than the host's local zone")
	}
	location, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, 0, 0, domain.NewError(domain.ErrClassConfiguration, "schedule_timezone_unknown",
			fmt.Sprintf("timezone %q is not a known IANA timezone name", s.Timezone))
	}
	return location, hour, minute, nil
}

// parseLocalTime accepts exactly "HH:MM" in 24-hour form. Anything looser — a
// missing leading zero, a seconds field, surrounding space — is rejected rather
// than normalised, because the stored value is compared as text elsewhere.
func parseLocalTime(value string) (int, int, error) {
	invalid := func() error {
		return domain.NewError(domain.ErrClassConfiguration, "schedule_time_invalid",
			fmt.Sprintf("%q is not a local time in 24-hour HH:MM form", value))
	}
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, invalid()
	}
	hour, err := strconv.Atoi(value[0:2])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, invalid()
	}
	minute, err := strconv.Atoi(value[3:5])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, invalid()
	}
	return hour, minute, nil
}

// NextAfter returns the first occurrence strictly after the given instant.
//
// "Strictly after" is what makes the caller's loop safe: passing the instant a
// schedule just fired at returns the following one rather than the same one
// again.
//
// ok is false, with no error, for a disabled schedule. A disabled schedule has
// no occurrences; it is not a failure to ask.
func (s Spec) NextAfter(after time.Time) (Occurrence, bool, error) {
	location, hour, minute, err := s.resolve()
	if err != nil {
		return Occurrence{}, false, err
	}
	if !s.Enabled {
		return Occurrence{}, false, nil
	}

	local := after.In(location)
	year, month, day := local.Date()
	// Anchored at noon so that adding days cannot land in a daylight-saving gap
	// and roll the calendar date.
	cursor := time.Date(year, month, day, 12, 0, 0, 0, location)

	// Today plus a full week is enough for any non-empty selection, including
	// the case where today's time has already passed and the only selected day
	// is today's weekday next week.
	for offset := 0; offset <= 7; offset++ {
		candidate := cursor.AddDate(0, 0, offset)
		if !s.Days.Contains(candidate.Weekday()) {
			continue
		}
		year, month, day := candidate.Date()
		instant, found := firstInstantOnDateAtOrAfter(location, year, month, day, hour, minute)
		if !found || !instant.After(after) {
			continue
		}
		return Occurrence{
			At: instant,
			Nominal: Nominal{
				Date: fmt.Sprintf("%04d-%02d-%02d", year, int(month), day),
				Time: s.LocalTime,
			},
		}, true, nil
	}
	// Unreachable for a validated spec: some day within eight is selected.
	return Occurrence{}, false, domain.NewError(domain.ErrClassInvariant, "schedule_no_occurrence",
		"no occurrence was found within eight days of a validated schedule")
}

// firstInstantOnDateAtOrAfter finds the earliest instant on one local calendar
// date whose wall clock has reached the requested time.
//
// Both daylight-saving edges fall out of that one definition, which is why it is
// phrased as "at or after" rather than "equal to":
//
//   - When the clocks go forward, the requested local time may not exist. The
//     earliest instant that has reached it is the moment the offset changed, so
//     a 02:30 schedule on a day that jumps 02:00 to 03:00 fires at 03:00 — the
//     first valid instant, not an hour later.
//   - When the clocks go back, the requested local time happens twice. The
//     earliest instant wins, so the schedule fires on the first pass and the
//     repeated hour cannot fire it again.
//
// The scan is minute by minute because that is the granularity of both the
// stored local time and every real transition, and because it makes no
// assumption about the size of the shift: a zone that moves by thirty or
// forty-five minutes behaves the same as one that moves by an hour.
func firstInstantOnDateAtOrAfter(location *time.Location, year int, month time.Month,
	day, hour, minute int) (time.Time, bool) {

	wanted := hour*60 + minute
	// Start well before local midnight and run past the end of the day, so a
	// transition at or near midnight is inside the window.
	start := time.Date(year, month, day, 12, 0, 0, 0, location).Add(-15 * time.Hour)
	for step := 0; step <= 27*60; step++ {
		instant := start.Add(time.Duration(step) * time.Minute)
		y, m, d := instant.Date()
		if y != year || m != month || d != day {
			continue
		}
		if instant.Hour()*60+instant.Minute() >= wanted {
			return instant, true
		}
	}
	return time.Time{}, false
}
