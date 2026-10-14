// Package app is yc-gateway's composition root.
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
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
	subscribers "github.com/slaghuis/yc-platform/services/yc-gateway/internal/transport/events"
	transport "github.com/slaghuis/yc-platform/services/yc-gateway/internal/transport/http"
	"github.com/slaghuis/yc-platform/services/yc-gateway/migrations"
)

// Run starts yc-gateway and blocks until ctx is cancelled.
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
	serveErr := httpkit.Serve(ctx, cfg.HTTP, a.Handler)

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return errors.Join(serveErr, a.Close(closeCtx))
}

// App is the wired service. Telemetry must be initialised first.
type App struct {
	Handler http.Handler
	Hosts   *service.Hosts
	closers []func(context.Context) error
}

// New migrates the database (when configured), loads the tenant hostname
// map, subscribes to tenant provisioning and builds the HTTP handler.
func New(ctx context.Context, cfg Config, build httpkit.BuildInfo) (_ *App, err error) {
	a := &App{}
	defer func() {
		if err != nil {
			_ = a.Close(context.WithoutCancel(ctx))
		}
	}()

	routes, err := cfg.routes()
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

	if a.Hosts, err = service.NewHosts(db, cfg.staticHosts()); err != nil {
		return nil, err
	}
	if err := a.Hosts.Load(ctx); err != nil {
		return nil, fmt.Errorf("load tenant hosts: %w", err)
	}
	bgCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { a.Hosts.Refresh(bgCtx, cfg.TenantHostsRefresh) })
	a.closers = append(a.closers, func(context.Context) error { stop(); wg.Wait(); return nil })

	validator, err := events.NewValidator()
	if err != nil {
		return nil, err
	}
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL:     cfg.NATS.URL,
		ServiceName: cfg.Service,
		DB:          db,
		Validator:   validator,
		Streams:     []eventbus.StreamConfig{eventbus.PlatformStream(), eventbus.DLQStream()},
	})
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, bus.Close)
	if err := bus.Subscribe(ctx, subscribers.TenantProvisionedSubscription,
		subscribers.TenantProvisioned(a.Hosts)); err != nil {
		return nil, err
	}

	identity := integration.NewIdentity(cfg.Upstreams[identityUpstream], cfg.IdentityTimeout, cfg.TokenCacheTTL, time.Now)
	gw := transport.New(a.Hosts, identity, routes, cfg.VisitorCookie)
	a.Handler = transport.NewRouter(gw, build, []httpkit.Check{
		{Name: "postgres", Fn: db.Ping},
		{Name: "nats", Fn: bus.Ping},
	})
	return a, nil
}

// Close stops subscribers and background work, then closes the database.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i](ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}
