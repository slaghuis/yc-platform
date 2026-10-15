// Package app is yc-cart's composition root.
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
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/service"
	subscribers "github.com/slaghuis/yc-platform/services/yc-cart/internal/transport/events"
	transport "github.com/slaghuis/yc-platform/services/yc-cart/internal/transport/http"
	"github.com/slaghuis/yc-platform/services/yc-cart/migrations"
)

// Run starts yc-cart and blocks until ctx is cancelled.
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

// New migrates the database (when configured), connects to Postgres and
// NATS, subscribes to login, preference and catalogue events and builds the
// HTTP handler. Prices come from yc-pricing through yc-gateway.
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
		Streams: append([]eventbus.StreamConfig{
			eventbus.CartStream(), eventbus.PreferenceStream(), eventbus.ProductStream(),
			eventbus.CheckoutStream(), eventbus.DLQStream(),
		}, eventbus.GDPRStreams()...),
	})
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, bus.Close)

	pricing := integration.NewPricing(httpkit.NewInternalClient(cfg.Service, cfg.InternalCalls))
	svc := service.New(db, bus, pricing, cfg.types(), service.Abandonment{After: cfg.Abandoned.After})
	a.Service = svc
	for _, sub := range subscribers.Subscriptions(svc) {
		if err := bus.Subscribe(ctx, sub.Opts, sub.Handler); err != nil {
			return nil, err
		}
	}

	bgCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { svc.RunAbandonedSweeper(bgCtx, cfg.Abandoned.SweepInterval) })
	a.closers = append(a.closers, func(context.Context) error { stop(); wg.Wait(); return nil })

	a.Handler = transport.NewRouter(svc, build, []httpkit.Check{
		{Name: "postgres", Fn: db.Ping},
		{Name: "nats", Fn: bus.Ping},
	})
	return a, nil
}

// Close stops the abandoned-cart sweeper and the event bus, then closes
// the database pool.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i](ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}
