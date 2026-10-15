package httpkit

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// ServerConfig is the `http:` block of every service config.
type ServerConfig struct {
	Listen          string        `koanf:"listen"`
	ReadTimeout     time.Duration `koanf:"read_timeout"`
	WriteTimeout    time.Duration `koanf:"write_timeout"`
	IdleTimeout     time.Duration `koanf:"idle_timeout"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
	// ShutdownDelay keeps serving for this long after the stop signal,
	// with /readyz answering 503, before the listener closes, so a load
	// balancer stops sending new requests first. 0 closes at once.
	ShutdownDelay time.Duration `koanf:"shutdown_delay"`
}

// DefaultServerConfig is the baseline services start from before loading
// config.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Listen:          ":8080",
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     60 * time.Second,
		ShutdownTimeout: 20 * time.Second,
	}
}

// Serve runs h until ctx is cancelled, then shuts down gracefully: /readyz
// turns unready for ShutdownDelay, then the listener closes and in-flight
// requests get up to ShutdownTimeout. It returns nil after a clean
// shutdown.
func Serve(ctx context.Context, cfg ServerConfig, h http.Handler) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("httpkit: listen %s: %w", cfg.Listen, err)
	}
	return ServeListener(ctx, cfg, ln, h)
}

// ServeListener is Serve on an existing listener; tests pass one bound to
// port 0.
func ServeListener(ctx context.Context, cfg ServerConfig, ln net.Listener, h http.Handler) error {
	var draining atomic.Bool
	srv := &http.Server{
		Handler:           unreadyWhile(&draining, h),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	logger := telemetry.LoggerFromContext(ctx)
	logger.Info("http server listening", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-errc:
		return fmt.Errorf("httpkit: serve: %w", err)
	case <-ctx.Done():
	}

	draining.Store(true)
	if cfg.ShutdownDelay > 0 {
		logger.Info("http server draining", slog.Duration("delay", cfg.ShutdownDelay))
		time.Sleep(cfg.ShutdownDelay)
	}
	logger.Info("http server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("httpkit: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !stderrors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("httpkit: serve: %w", err)
	}
	return nil
}

// unreadyWhile answers /readyz with 503 while draining is set.
func unreadyWhile(draining *atomic.Bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() && r.URL.Path == pathReadyz {
			WriteJSON(w, http.StatusServiceUnavailable, Readiness{Status: "draining", Checks: map[string]string{}})
			return
		}
		h.ServeHTTP(w, r)
	})
}
