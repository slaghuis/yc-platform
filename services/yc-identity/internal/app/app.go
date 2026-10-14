// Package app is yc-identity's composition root.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/service"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/token"
	transport "github.com/slaghuis/yc-platform/services/yc-identity/internal/transport/http"
	"github.com/slaghuis/yc-platform/services/yc-identity/migrations"
)

// Run starts yc-identity and blocks until ctx is cancelled.
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
	closers []func(context.Context) error
}

// New migrates the database (when configured), connects to Postgres and
// NATS, and builds the HTTP handler.
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
		NATSURL:      cfg.NATS.URL,
		ServiceName:  cfg.Service,
		DB:           db,
		DisableRelay: cfg.NATS.DisableRelay,
		Validator:    validator,
		Streams:      []eventbus.StreamConfig{eventbus.IdentityStream(), eventbus.DLQStream()},
	})
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, bus.Close)

	key, err := token.LoadPrivateKey(cfg.JWT.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	signer, err := token.NewSigner(key, cfg.JWT.Issuer, time.Now)
	if err != nil {
		return nil, err
	}

	svc := service.New(db, bus, signer, service.Config{
		AccessTokenTTL:  cfg.JWT.AccessTokenTTL,
		VisitorTokenTTL: cfg.JWT.VisitorTokenTTL,
	})
	a.Handler = transport.NewRouter(svc, build, []httpkit.Check{
		{Name: "postgres", Fn: db.Ping},
		{Name: "nats", Fn: bus.Ping},
	})
	return a, nil
}

// Close stops the event bus, then closes the database pool.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i](ctx))
	}
	a.closers = nil
	return errors.Join(errs...)
}
