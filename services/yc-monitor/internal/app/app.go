// Package app is yc-monitor's composition root.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/service"
	subscribers "github.com/slaghuis/yc-platform/services/yc-monitor/internal/transport/events"
	transport "github.com/slaghuis/yc-platform/services/yc-monitor/internal/transport/http"
	"github.com/slaghuis/yc-platform/services/yc-monitor/migrations"
)

// Run starts yc-monitor and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config, build httpkit.BuildInfo) error {
	shutdownTelemetry, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:    cfg.Service,
		ServiceVersion: build.Version,
		Environment:    cfg.Environment,
		OTLPEndpoint:   cfg.Telemetry.OTLPEndpoint,
		LogLevel:       cfg.Log.Level,
	})
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(flushCtx)
	}()

	a, err := New(ctx, cfg, build)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		httpkit.KeepRegistered(ctx, cfg.Gateway, httpkit.Registration{
			ServiceName: cfg.Service, Version: build.Version, Routes: transport.Routes(),
		})
	})
	serveErr := httpkit.Serve(ctx, cfg.HTTP, a.Handler)
	wg.Wait()

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return errors.Join(serveErr, a.Close(closeCtx))
}

// App is the wired service. Telemetry must be initialised first.
type App struct {
	Handler http.Handler
	Service *service.Service
	closers []func(context.Context) error
}

// New migrates the database (when configured), subscribes to the events
// yc-monitor counts and alerts on, starts the health checks, alert
// evaluation and retention purge, and builds the HTTP handler.
func New(ctx context.Context, cfg Config, build httpkit.BuildInfo) (_ *App, err error) {
	a := &App{}
	defer func() {
		if err != nil {
			_ = a.Close(context.WithoutCancel(ctx))
		}
	}()

	if cfg.Database.MigrateOnStart {
		if err := dbkit.Migrate(cfg.Database.URL, migrations.FS); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	db, err := dbkit.Connect(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, func(context.Context) error { db.Close(); return nil })

	validator, err := events.NewValidator()
	if err != nil {
		return nil, err
	}
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL:     cfg.NATS.URL,
		ServiceName: cfg.Service,
		DB:          db,
		Validator:   validator,
		Streams: []eventbus.StreamConfig{
			eventbus.IdentityStream(), eventbus.PlatformStream(), eventbus.MonitorStream(), eventbus.CheckoutStream(),
			eventbus.OrderStream(), eventbus.PaymentStream(), eventbus.PricingStream(), eventbus.AuditStream(),
			eventbus.DLQStream(),
		},
	})
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, bus.Close)

	a.Service = service.New(db, bus, cfg.settings())
	for _, sub := range subscribers.Subscriptions(a.Service, cfg.Alerts.CriticalWebhookURL != "") {
		if err := bus.Subscribe(ctx, sub.Opts, sub.Handler); err != nil {
			return nil, err
		}
	}
	if err := bus.SubscribeDLQ(ctx, subscribers.DLQSubscription, subscribers.DeadLetter(a.Service)); err != nil {
		return nil, err
	}

	bgCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { a.Service.RunHealthChecks(bgCtx, cfg.HealthCheck.Interval) })
	wg.Go(func() { a.Service.RunEvaluator(bgCtx, cfg.Alerts.EvaluateInterval) })
	wg.Go(func() { a.Service.RunPurger(bgCtx, cfg.Retention.PurgeInterval) })
	a.closers = append(a.closers, func(context.Context) error { stop(); wg.Wait(); return nil })

	a.Handler = transport.NewRouter(a.Service, build, []httpkit.Check{
		{Name: "postgres", Fn: db.Ping},
		{Name: "nats", Fn: bus.Ping},
	})
	return a, nil
}

// Close stops the background loops and the event bus, then closes the
// database pool.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i](ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}
