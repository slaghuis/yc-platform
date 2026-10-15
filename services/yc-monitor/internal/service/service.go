// Package service holds yc-monitor's use cases: health-checking registered
// services, counting business and dead-letter events, raising and routing
// alerts, and the dashboards (docs/services/yc-monitor.md, ADR-025).
package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/repository"
)

// ServiceName is the actor ID of events yc-monitor publishes.
const ServiceName = "yc-monitor"

// MaxLimit caps a listing.
const MaxLimit = 500

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Config tunes the service.
type Config struct {
	Health        domain.HealthPolicy
	HealthTimeout time.Duration
	// Defaults are the threshold rules' settings when no override is set.
	Defaults map[string]domain.Thresholds
	// Retention per rollup bucket size; dead letters and received events
	// are kept for DLQRetention and EventRetention.
	Retention      repository.Retention
	DLQRetention   time.Duration
	EventRetention time.Duration
	// CriticalWebhookURL, when set, receives every critical alert.
	CriticalWebhookURL string
}

// Service implements the monitor use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	cfg      Config
	client   *http.Client
	now      func() time.Time
	events   repository.Events
	dlq      repository.DLQ
	services repository.Services
	metrics  repository.Metrics
	alerts   repository.Alerts
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, cfg Config) *Service {
	return &Service{
		db: db, bus: bus, cfg: cfg, now: time.Now,
		client: &http.Client{Timeout: cfg.HealthTimeout},
	}
}

// RecordEvent stores a consumed event under its tenant.
func (s *Service) RecordEvent(ctx context.Context, e domain.ReceivedEvent) error {
	return tenantTx(ctx, s.db, e.TenantID, func(tx pgx.Tx) error {
		return s.events.Record(ctx, tx, e)
	})
}

// RecordDeadLetter stores a dead letter under the DLQ subject's tenant and
// counts it once.
func (s *Service) RecordDeadLetter(ctx context.Context, d domain.DLQEntry) error {
	return tenantTx(ctx, s.db, d.TenantID, func(tx pgx.Tx) error {
		inserted, err := s.dlq.Record(ctx, tx, d)
		if err != nil || !inserted {
			return err
		}
		return s.metrics.Increment(ctx, tx, domain.Count{
			TenantID: d.TenantID, Metric: domain.MetricDeadLetters,
			Dimensions: map[string]string{"service": d.Service}, At: s.now().UTC(),
		})
	})
}

// ListEvents returns received events across tenants (system admin).
func (s *Service) ListEvents(ctx context.Context, f domain.Filter) ([]domain.ReceivedEvent, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	f, err := clamp(f)
	if err != nil {
		return nil, err
	}
	var out []domain.ReceivedEvent
	err = dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.events.List(ctx, tx, f)
		return err
	})
	return out, err
}

// ListDeadLetters returns dead letters across tenants (system admin).
func (s *Service) ListDeadLetters(ctx context.Context, f domain.Filter) ([]domain.DLQEntry, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	f, err := clamp(f)
	if err != nil {
		return nil, err
	}
	var out []domain.DLQEntry
	err = dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.dlq.List(ctx, tx, f)
		return err
	})
	return out, err
}

// AcknowledgeDeadLetter marks a dead letter handled, e.g. after a replay
// with yc-dlq-cli (system admin).
func (s *Service) AcknowledgeDeadLetter(ctx context.Context, sequence uint64) (domain.DLQEntry, error) {
	var d domain.DLQEntry
	if err := requireSystemAdmin(ctx); err != nil {
		return d, err
	}
	a, _ := authctx.FromContext(ctx)
	var tenantID string
	var ok bool
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		tenantID, ok, err = s.dlq.TenantOf(ctx, tx, sequence)
		return err
	})
	if err != nil {
		return d, err
	}
	if !ok {
		return d, ycerrors.NotFound("Dead letter", fmt.Sprint(sequence))
	}
	err = tenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		d, _, err = s.dlq.Acknowledge(ctx, tx, sequence, a.ActorID, s.now().UTC())
		return err
	})
	return d, err
}

// DeadLetterSummary counts unacknowledged dead letters per service, for
// every shop (system admin) or the caller's (shop admin).
func (s *Service) DeadLetterSummary(ctx context.Context) ([]domain.DLQSummary, error) {
	var out []domain.DLQSummary
	err := s.readScoped(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.dlq.Summary(ctx, tx, tenantID)
		return err
	})
	return out, err
}

// readScoped runs fn for a system admin over every tenant (tenantID "")
// or for a shop admin over their shop.
func (s *Service) readScoped(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	switch a.ActorType {
	case authctx.ActorSystemAdmin:
		return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error { return fn(tx, "") })
	case authctx.ActorShopAdmin:
		tenantID, err := shopAdminTenant(ctx)
		if err != nil {
			return err
		}
		return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
	case authctx.ActorVisitor, authctx.ActorCustomer, authctx.ActorService:
	}
	return ycerrors.Forbidden("Admins only").WithInternal("actor_type %s", a.ActorType)
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID string, payload interface{ EventType() string }) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: payload.EventType(), TenantID: tenantID,
		Actor:   eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName},
		Payload: payload,
	})
}

// tenantTx writes under tenantID's RLS context. Platform rows are written
// in the platform context, which the policies admit only for _platform rows.
func tenantTx(ctx context.Context, db *pgxpool.Pool, tenantID string, fn dbkit.TxFunc) error {
	if tenantID == dbkit.PlatformTenantID {
		return dbkit.WithPlatformTx(ctx, db, fn)
	}
	return dbkit.WithTenantTx(ctx, db, tenantID, fn)
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

// clamp applies the default limit and refuses one out of range.
func clamp(f domain.Filter) (domain.Filter, error) {
	if f.Limit < 0 || f.Limit > MaxLimit {
		return f, ycerrors.Validation().
			Field("limit", "out_of_range", fmt.Sprintf("limit must be between 1 and %d", MaxLimit)).Build()
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	return f, nil
}
