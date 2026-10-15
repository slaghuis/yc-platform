package domain

import "time"

// HealthStatus is a registered service's health.
type HealthStatus string

// Health statuses. A deregistered service is no longer checked.
const (
	HealthUnknown      HealthStatus = "unknown"
	HealthHealthy      HealthStatus = "healthy"
	HealthUnhealthy    HealthStatus = "unhealthy"
	HealthDeregistered HealthStatus = "deregistered"
)

// Service is a service registered with yc-gateway.
type Service struct {
	Name                 string       `json:"name"`
	InstanceID           string       `json:"instance_id"`
	BaseURL              string       `json:"base_url"`
	Version              string       `json:"version,omitempty"`
	Status               HealthStatus `json:"status"`
	ConsecutiveFailures  int          `json:"consecutive_failures"`
	ConsecutiveSuccesses int          `json:"consecutive_successes"`
	LastError            string       `json:"last_error,omitempty"`
	LastCheckAt          *time.Time   `json:"last_check_at,omitempty"`
	LastHealthyAt        *time.Time   `json:"last_healthy_at,omitempty"`
	UnhealthySince       *time.Time   `json:"unhealthy_since,omitempty"`
	RegisteredAt         time.Time    `json:"registered_at"`
	DeregisteredAt       *time.Time   `json:"deregistered_at,omitempty"`
}

// HealthPolicy is how many checks in a row change a service's health.
type HealthPolicy struct {
	FailuresToUnhealthy int
	SuccessesToRecover  int
}

// Transition is what a health check changed.
type Transition int

// Transitions.
const (
	NoTransition Transition = iota
	BecameHealthy
	BecameUnhealthy
	Recovered
)

// Observe applies a readiness check's outcome (errText is empty when it
// passed) and reports the transition.
func (s *Service) Observe(errText string, now time.Time, p HealthPolicy) Transition {
	s.LastCheckAt = &now
	if errText != "" {
		s.ConsecutiveFailures++
		s.ConsecutiveSuccesses = 0
		s.LastError = errText
		if s.Status != HealthUnhealthy && s.ConsecutiveFailures >= p.FailuresToUnhealthy {
			s.Status, s.UnhealthySince = HealthUnhealthy, &now
			return BecameUnhealthy
		}
		return NoTransition
	}
	s.ConsecutiveSuccesses++
	s.ConsecutiveFailures = 0
	s.LastHealthyAt = &now
	switch s.Status {
	case HealthUnhealthy:
		if s.ConsecutiveSuccesses >= p.SuccessesToRecover {
			s.Status, s.LastError, s.UnhealthySince = HealthHealthy, "", nil
			return Recovered
		}
	case HealthUnknown:
		s.Status, s.LastError = HealthHealthy, ""
		return BecameHealthy
	case HealthHealthy, HealthDeregistered:
		s.LastError = ""
	}
	return NoTransition
}
