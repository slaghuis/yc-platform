// Package service holds yc-audit's use cases: recording audited events,
// tracking GDPR deletions to completion or incident, and reading and
// exporting the log (ADR-025).
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/repository"
)

// ServiceName is the actor ID of events yc-audit publishes.
const ServiceName = "yc-audit"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Config tunes the service.
type Config struct {
	// ExpectedServices must each report a GDPR deletion.
	ExpectedServices []string
	// Deadline is how long a deletion may take before it is an incident.
	Deadline time.Duration
	// MaxExport caps an export.
	MaxExport int
}

// Service implements the audit use cases.
type Service struct {
	db   *pgxpool.Pool
	bus  Publisher
	repo repository.Audit
	cfg  Config
	now  func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, cfg Config) *Service {
	return &Service{db: db, bus: bus, cfg: cfg, now: time.Now}
}

// Record stores an audited event. Repeats are ignored; event types without
// a rule are refused as invalid.
func (s *Service) Record(ctx context.Context, env eventbus.Envelope) error {
	return s.inAnyTenant(ctx, env.TenantID, func(tx pgx.Tx, tenantID string) error {
		return s.record(ctx, tx, tenantID, env)
	})
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, tenantID string, env eventbus.Envelope) error {
	rule, ok := domain.Rules[env.EventType]
	if !ok {
		return ycerrors.Validation().Field("event_type", "unsupported", env.EventType+" is not audited").Build()
	}
	var p map[string]any
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return ycerrors.Validation().Field("payload", "invalid", "Payload is not an object").Wrap(err)
	}
	subject, summary := rule.Describe(p)
	_, err := s.repo.InsertRecord(ctx, tx, domain.Record{
		ID: uuid.NewString(), TenantID: tenantID, Category: rule.Category, EventType: env.EventType,
		EventID: env.EventID, SourceService: env.SourceService, ActorType: env.Actor.Type, ActorID: env.Actor.ID,
		SubjectRef: subject, Summary: summary, CorrelationID: env.CorrelationID, Payload: env.Payload,
		EventTime: env.EventTime, RecordedAt: s.now().UTC(),
	})
	return err
}

// UserDeleted records a deletion and opens its register entry with the
// configured services and deadline.
func (s *Service) UserDeleted(ctx context.Context, env eventbus.Envelope, p events.IdentityUserDeletedV1) error {
	return s.inShop(ctx, env.TenantID, func(tx pgx.Tx, tenantID string) error {
		if err := s.record(ctx, tx, tenantID, env); err != nil {
			return err
		}
		if _, err := s.repo.OpenRegister(ctx, tx, domain.Register{
			TenantID: tenantID, UserID: p.UserID, Pseudonym: p.Pseudonym, Role: p.Role, RequestedBy: p.RequestedBy,
			InitiatedAt: p.DeletedAt, Deadline: p.DeletedAt.Add(s.cfg.Deadline), Expected: s.cfg.ExpectedServices,
		}); err != nil {
			return err
		}
		return s.settle(ctx, tx, tenantID, p.UserID)
	})
}

// CascadeCompleted records a service's report on a deletion.
func (s *Service) CascadeCompleted(ctx context.Context, env eventbus.Envelope, p events.PlatformGdprCascadeCompletedV1) error {
	return s.inShop(ctx, env.TenantID, func(tx pgx.Tx, tenantID string) error {
		if err := s.record(ctx, tx, tenantID, env); err != nil {
			return err
		}
		if _, err := s.repo.AddReport(ctx, tx, tenantID, p.UserID, domain.Report{
			Service: p.Service, Action: p.Action, RecordCount: p.RecordCount, CompletedAt: p.CompletedAt,
			ReportedAt: s.now().UTC(),
		}); err != nil {
			return err
		}
		return s.settle(ctx, tx, tenantID, p.UserID)
	})
}

// settle completes a deletion once every expected service has reported.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, tenantID, userID string) error {
	reg, ok, err := s.repo.Register(ctx, tx, tenantID, userID, true)
	if err != nil || !ok || !reg.Completes() {
		return err
	}
	now := s.now().UTC()
	reg.Status, reg.CompletedAt = domain.RegisterComplete, &now
	if err := s.repo.SetRegisterStatus(ctx, tx, reg); err != nil {
		return err
	}
	services := make([]string, len(reg.Reports))
	for i, r := range reg.Reports {
		services[i] = r.Service
	}
	return s.publish(ctx, tx, tenantID, events.AuditGdprCascadeCompleteV1{
		UserID: reg.UserID, Pseudonym: reg.Pseudonym, Services: services, InitiatedAt: reg.InitiatedAt, CompletedAt: now,
	})
}

// SweepIncidents marks deletions past their deadline as incidents and
// announces each, so yc-monitor raises a critical alert. It returns how
// many it marked.
func (s *Service) SweepIncidents(ctx context.Context) (int, error) {
	var due []repository.RegisterKey
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.Overdue(ctx, tx, s.now().UTC(), 100)
		return err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range due {
		err := s.inShop(ctx, k.TenantID, func(tx pgx.Tx, tenantID string) error {
			reg, ok, err := s.repo.Register(ctx, tx, tenantID, k.UserID, true)
			now := s.now().UTC()
			if err != nil || !ok || !reg.Overdue(now) {
				return err
			}
			reg.Status, reg.IncidentAt = domain.RegisterIncident, &now
			if err := s.repo.SetRegisterStatus(ctx, tx, reg); err != nil {
				return err
			}
			telemetry.LoggerFromContext(ctx).Error("GDPR deletion missed its deadline",
				"tenant_id", tenantID, "user_id", reg.UserID, "missing_services", reg.Missing(), "deadline", reg.Deadline)
			n++
			return s.publish(ctx, tx, tenantID, events.AuditGdprCascadeIncidentV1{
				UserID: reg.UserID, Pseudonym: reg.Pseudonym, MissingServices: reg.Missing(),
				InitiatedAt: reg.InitiatedAt, Deadline: reg.Deadline,
			})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// RunSweeper sweeps for incidents every interval until ctx ends.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.SweepIncidents(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Warn("GDPR incident sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID string, payload interface{ EventType() string }) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: payload.EventType(), TenantID: tenantID,
		Actor:   eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName},
		Payload: payload,
	})
}

// inShop runs fn in a shop's tenant context.
func (s *Service) inShop(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if tenantID == eventbus.PlatformTenantID {
		return ycerrors.Validation().Field("tenant_id", "invalid", "A GDPR deletion belongs to a shop").Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

// inAnyTenant runs fn in a shop's context, or the platform's for platform
// events. Writes always name the tenant, so the platform context's view of
// every shop does not leak into them.
func (s *Service) inAnyTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if tenantID == eventbus.PlatformTenantID {
		return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error { return fn(tx, tenantID) })
	}
	return s.inShop(ctx, tenantID, fn)
}

// requireSystemAdmin refuses other callers.
func requireSystemAdmin(ctx context.Context) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return ycerrors.Forbidden("System admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return nil
}

// shopAdminTenant returns the calling shop admin's shop.
func shopAdminTenant(ctx context.Context) (string, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return "", ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if a.TenantID == "" || a.TenantID == eventbus.PlatformTenantID {
		return "", ycerrors.Forbidden("Shop admins only").WithInternal("tenant %q", a.TenantID)
	}
	return a.TenantID, nil
}

func wrapf(format string, args ...any) error { return fmt.Errorf(format, args...) }
