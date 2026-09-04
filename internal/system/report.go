// Package system inspects and provisions the Linux integration ROTUP needs
// around itself: the privileged mount helper, its narrow sudo policy, the
// volatile runtime directory, the persistent data locations and the service
// unit.
//
// The package is split deliberately. Everything that decides what is correct is
// a pure function over an observed state, so it is testable without root and on
// any platform. Everything that touches the machine goes through the Host seam,
// which is small enough to read in one sitting and to substitute in tests.
//
// Two entry points use it and their contract is different:
//
//   - Inspect is read-only. It never creates, moves, chowns or starts anything.
//   - Provision may create and repair the integration it owns, and reports
//     every change it made. It is idempotent: a second run of a converged
//     system reports no changes.
//
// Neither reads, writes, or prints secret material. The secret store is only
// ever checked for existence and permission bits, never opened.
package system

import "sort"

// Status is the outcome of one check. The four values are ordered by how much
// operator attention they demand, which is what Worst compares.
type Status string

const (
	// StatusOK means the check passed and nothing was changed.
	StatusOK Status = "OK"
	// StatusFixed means provisioning repaired something. Inspect never returns
	// it, because inspection changes nothing.
	StatusFixed Status = "FIXED"
	// StatusWarning means ROTUP will run, but something is worth knowing:
	// a degraded capability, or a configuration a production host should not
	// keep.
	StatusWarning Status = "WARNING"
	// StatusError means this part of the integration is not usable as it is.
	StatusError Status = "ERROR"
)

var statusRank = map[Status]int{StatusOK: 0, StatusFixed: 1, StatusWarning: 2, StatusError: 3}

// Check is one inspected or provisioned aspect of the installation.
type Check struct {
	// ID is stable across releases so scripts and support can match on it.
	ID string `json:"id"`
	// Group buckets checks for human-readable output.
	Group string `json:"group"`
	// Title is the short human name of what was checked.
	Title string `json:"title"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Detail states what was actually observed. It names exact paths, modes and
	// versions, and never contains secret material.
	Detail string `json:"detail"`
	// Remedy is the operator's next step when Status is not OK or FIXED.
	Remedy string `json:"remedy,omitempty"`
}

// Report accumulates checks in the order they were performed.
type Report struct {
	Checks []Check `json:"checks"`
}

// Add appends a check.
func (r *Report) Add(c Check) { r.Checks = append(r.Checks, c) }

// OK appends a passing check.
func (r *Report) OK(group, id, title, detail string) {
	r.Add(Check{ID: id, Group: group, Title: title, Status: StatusOK, Detail: detail})
}

// Fixed appends a check recording a repair that provisioning performed.
func (r *Report) Fixed(group, id, title, detail string) {
	r.Add(Check{ID: id, Group: group, Title: title, Status: StatusFixed, Detail: detail})
}

// Warn appends a non-fatal finding.
func (r *Report) Warn(group, id, title, detail, remedy string) {
	r.Add(Check{ID: id, Group: group, Title: title, Status: StatusWarning, Detail: detail, Remedy: remedy})
}

// Fail appends a finding that makes this part of the integration unusable.
func (r *Report) Fail(group, id, title, detail, remedy string) {
	r.Add(Check{ID: id, Group: group, Title: title, Status: StatusError, Detail: detail, Remedy: remedy})
}

// Worst returns the most demanding status in the report, or StatusOK when it is
// empty.
func (r Report) Worst() Status {
	worst := StatusOK
	for _, c := range r.Checks {
		if statusRank[c.Status] > statusRank[worst] {
			worst = c.Status
		}
	}
	return worst
}

// Ready reports whether the system can be used. A warning is deliberately not
// disqualifying: a host with restic missing can still collect sources, and
// saying "not ready" would hide which half actually works.
func (r Report) Ready() bool {
	for _, c := range r.Checks {
		if c.Status == StatusError {
			return false
		}
	}
	return true
}

// Changed reports whether provisioning altered anything, which is what makes a
// rerun observable as a no-op.
func (r Report) Changed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFixed {
			return true
		}
	}
	return false
}

// Failures returns the checks an operator has to act on, most severe first and
// otherwise in the order they were found.
func (r Report) Failures() []Check {
	var found []Check
	for _, c := range r.Checks {
		if c.Status == StatusWarning || c.Status == StatusError {
			found = append(found, c)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		return statusRank[found[i].Status] > statusRank[found[j].Status]
	})
	return found
}

// Groups the report uses. They are constants so output ordering and the API can
// rely on them.
const (
	GroupPlatform = "System"
	GroupTools    = "External tools"
	GroupHelper   = "Privileged media helper"
	GroupRuntime  = "Runtime directory"
	GroupData     = "Application data"
	GroupService  = "Daemon service"
)
