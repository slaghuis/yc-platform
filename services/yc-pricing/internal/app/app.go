// Package app is yc-pricing's composition root.
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
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/service"
	subscribers "github.com/slaghuis/yc-platform/services/yc-pricing/internal/transport/events"
	transport "github.com/slaghuis/yc-platform/services/yc-pricing/internal/transport/http"
	"github.com/slaghuis/yc-platform/services/yc-pricing/migrations"
)

// Run starts yc-pricing and blocks until ctx is cancelled.
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
	Rates   *service.Rates
	closers []func(context.Context) error
}

// New migrates the database (when configured), connects to Postgres and
// NATS, subscribes to catalog and tenant events, starts the exchange rate
// refresher and builds the HTTP handler.
func New(ctx context.Context, cfg Config, build httpkit.BuildInfo) (_ *App, err error) {
	a := &App{}
	defer func() {
		if err != nil {
			_ = a.Close(context.WithoutCancel(ctx))
		}
	}()

	provider, err := integration.NewRateProvider(cfg.Rates.provider())
	if err != nil {
		return nil, err
	}
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
			eventbus.PricingStream(), eventbus.ProductStream(), eventbus.PlatformStream(), eventbus.DLQStream(),
		},
	})
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, bus.Close)

	svc := service.New(db, bus)
	a.Rates = service.NewRates(db, bus, provider, cfg.Rates.refresh())
	for _, sub := range append(subscribers.Subscriptions(svc), subscribers.RatesSubscription(a.Rates)) {
		if err := bus.Subscribe(ctx, sub.Opts, sub.Handler); err != nil {
			return nil, err
		}
	}

	ratesCtx, stopRates := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { a.Rates.Run(ratesCtx) })
	a.closers = append(a.closers, func(context.Context) error { stopRates(); wg.Wait(); return nil })

	a.Handler = transport.NewRouter(svc, build, []httpkit.Check{
		{Name: "postgres", Fn: db.Ping},
		{Name: "nats", Fn: bus.Ping},
	})
	return a, nil
}

// Close stops the rate refresher and the event bus, then closes the
// database pool.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i](ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}
