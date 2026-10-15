package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// ServiceRegistered starts (or resumes) health-checking a service.
func (s *Service) ServiceRegistered(ctx context.Context, p events.PlatformServiceRegisteredV1) error {
	version := ""
	if p.Version != nil {
		version = *p.Version
	}
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		return s.services.Register(ctx, tx, p.ServiceName, p.InstanceID, strings.TrimRight(p.BaseURL, "/"),
			version, p.RegisteredAt)
	})
}

// ServiceDeregistered stops checking a service that shut down or was
// removed. yc-gateway also deregisters services that fail its own health
// checks; those stay checked so their outage is alerted on.
func (s *Service) ServiceDeregistered(ctx context.Context, p events.PlatformServiceDeregisteredV1) error {
	if p.Reason == events.PlatformServiceDeregisteredV1ReasonHealthCheckFailed {
		return nil
	}
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		svc, ok, err := s.services.Get(ctx, tx, p.ServiceName)
		if err != nil || !ok || svc.InstanceID != p.InstanceID || svc.RegisteredAt.After(p.DeregisteredAt) {
			return err
		}
		at := p.DeregisteredAt
		svc.Status, svc.DeregisteredAt = domain.HealthDeregistered, &at
		svc.ConsecutiveFailures, svc.ConsecutiveSuccesses = 0, 0
		if err := s.services.Save(ctx, tx, svc); err != nil {
			return err
		}
		return s.resolve(ctx, tx, eventbus.PlatformTenantID, domain.RuleServiceUnhealthy,
			map[string]string{"service": svc.Name})
	})
}

// Services lists the registered services and their health (system admin).
func (s *Service) Services(ctx context.Context) ([]domain.Service, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	var out []domain.Service
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.services.List(ctx, tx)
		return err
	})
	return out, err
}

// CheckHealth checks every registered service's /readyz once, in parallel,
// and applies the outcomes.
func (s *Service) CheckHealth(ctx context.Context) error {
	var all []domain.Service
	if err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		all, err = s.services.List(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(all))
	for i, svc := range all {
		if svc.Status == domain.HealthDeregistered {
			continue
		}
		wg.Go(func() { errs[i] = s.observe(ctx, svc.Name, svc.InstanceID, s.probe(ctx, svc.BaseURL)) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// RunHealthChecks checks every interval until ctx ends.
func (s *Service) RunHealthChecks(ctx context.Context, interval time.Duration) {
	every(ctx, interval, "health check", s.CheckHealth)
}

// probe returns why baseURL/readyz failed, or "" when it passed.
func (s *Service) probe(ctx context.Context, baseURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/readyz", nil)
	if err != nil {
		return err.Error()
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err.Error()
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("readyz returned %d", resp.StatusCode)
	}
	return ""
}

func (s *Service) observe(ctx context.Context, name, instanceID, errText string) error {
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		svc, ok, err := s.services.Get(ctx, tx, name)
		if err != nil || !ok || svc.InstanceID != instanceID || svc.Status == domain.HealthDeregistered {
			return err
		}
		now := s.now().UTC()
		lastHealthy, since := svc.LastHealthyAt, svc.UnhealthySince
		transition := svc.Observe(errText, now, s.cfg.Health)
		if err := s.services.Save(ctx, tx, svc); err != nil {
			return err
		}
		dims := map[string]string{"service": svc.Name}
		log := telemetry.LoggerFromContext(ctx)
		switch transition {
		case domain.BecameUnhealthy:
			log.Error("service unhealthy", "monitored_service", svc.Name, "failures", svc.ConsecutiveFailures,
				"last_error", svc.LastError)
			if err := s.publish(ctx, tx, eventbus.PlatformTenantID, events.MonitorServiceUnhealthyV1{
				ServiceName: svc.Name, ConsecutiveFailures: int64(svc.ConsecutiveFailures), LastError: svc.LastError,
				LastHealthyAt: lastHealthy, DetectedAt: now,
			}); err != nil {
				return err
			}
			summary := fmt.Sprintf("%s failed %d health checks in a row: %s", svc.Name, svc.ConsecutiveFailures,
				svc.LastError)
			return s.raise(ctx, tx, eventbus.PlatformTenantID, domain.RuleServiceUnhealthy, dims, summary, nil, nil)
		case domain.Recovered:
			log.Info("service recovered", "monitored_service", svc.Name)
			if err := s.publish(ctx, tx, eventbus.PlatformTenantID, events.MonitorServiceRecoveredV1{
				ServiceName: svc.Name, UnhealthySince: deref(since, now), RecoveredAt: now,
			}); err != nil {
				return err
			}
			return s.resolve(ctx, tx, eventbus.PlatformTenantID, domain.RuleServiceUnhealthy, dims)
		case domain.NoTransition, domain.BecameHealthy:
		}
		return nil
	})
}

// every runs fn now and then every interval until ctx ends, logging
// failures.
func every(ctx context.Context, interval time.Duration, what string, fn func(context.Context) error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Warn(what+" failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func deref[T any](p *T, otherwise T) T {
	if p == nil {
		return otherwise
	}
	return *p
}
