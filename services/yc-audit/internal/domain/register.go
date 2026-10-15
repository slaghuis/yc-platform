package domain

import (
	"slices"
	"time"
)

// RegisterStatus is where a GDPR deletion is.
type RegisterStatus string

// Register statuses. An incident becomes complete if the missing services
// report late; IncidentAt keeps the breach on record.
const (
	RegisterInProgress RegisterStatus = "in_progress"
	RegisterComplete   RegisterStatus = "complete"
	RegisterIncident   RegisterStatus = "incident"
)

// Valid reports whether s is a known status.
func (s RegisterStatus) Valid() bool {
	switch s {
	case RegisterInProgress, RegisterComplete, RegisterIncident:
		return true
	default:
		return false
	}
}

// Register is one user's GDPR deletion across the platform (ADR-005).
type Register struct {
	TenantID    string
	UserID      string
	Pseudonym   string
	Role        string
	RequestedBy string
	InitiatedAt time.Time
	Deadline    time.Time
	// Expected are the services that must report, from configuration when
	// the deletion arrived.
	Expected    []string
	Status      RegisterStatus
	CompletedAt *time.Time
	IncidentAt  *time.Time
	Reports     []Report
}

// Report is a service's part of a deletion.
type Report struct {
	Service     string
	Action      string
	RecordCount int64
	CompletedAt time.Time
	ReportedAt  time.Time
}

// Missing returns the expected services that have not reported.
func (r Register) Missing() []string {
	var out []string
	for _, s := range r.Expected {
		if !slices.ContainsFunc(r.Reports, func(rep Report) bool { return rep.Service == s }) {
			out = append(out, s)
		}
	}
	return out
}

// Completes reports whether the register should now become complete.
func (r Register) Completes() bool {
	return r.Status != RegisterComplete && len(r.Missing()) == 0
}

// Overdue reports whether the deletion missed its deadline at now.
func (r Register) Overdue(now time.Time) bool {
	return r.Status == RegisterInProgress && now.After(r.Deadline) && len(r.Missing()) > 0
}
