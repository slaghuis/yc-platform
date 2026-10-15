package service

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
)

var serviceNameRe = regexp.MustCompile(`^yc-[a-z]+(-[a-z]+)*$`)

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// RegistryConfig tunes health polling.
type RegistryConfig struct {
	HealthInterval   time.Duration `koanf:"health_interval"`
	HealthTimeout    time.Duration `koanf:"health_timeout"`
	FailureThreshold int           `koanf:"failure_threshold"`
}

type registration struct {
	req      httpkit.Registration
	routes   []domain.Route
	health   string
	failures int
}

// Registry is the routing table: static routes from config plus service
// registrations held in memory (ADR-020 rule 7). Match never blocks.
type Registry struct {
	static   []domain.Route
	prefixes map[string]bool
	db       *pgxpool.Pool
	bus      Publisher
	cfg      RegistryConfig
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	regs  map[string]*registration // by service name
	table atomic.Pointer[domain.Routes]
}

// NewRegistry returns a registry serving only the static routes.
func NewRegistry(static []domain.Route, db *pgxpool.Pool, bus Publisher, cfg RegistryConfig) (*Registry, error) {
	r := &Registry{
		static:   static,
		prefixes: map[string]bool{},
		db:       db,
		bus:      bus,
		cfg:      cfg,
		client:   &http.Client{Timeout: cfg.HealthTimeout},
		now:      time.Now,
		regs:     map[string]*registration{},
	}
	for _, s := range static {
		r.prefixes[s.Prefix] = true
	}
	t, err := domain.NewRoutes(static)
	if err != nil {
		return nil, err
	}
	r.table.Store(t)
	return r, nil
}

// Match returns the route for a request.
func (r *Registry) Match(method, path string) (domain.Route, bool) {
	return r.table.Load().Match(method, path)
}

// MatchInternal returns the registered route for a service-to-service
// call.
func (r *Registry) MatchInternal(method, path string) (domain.Route, bool) {
	return r.table.Load().MatchInternal(method, path)
}

// Routes returns every route, longest prefix first.
func (r *Registry) Routes() []domain.Route {
	return r.table.Load().All()
}

// Register validates req and replaces the service's routes. An unchanged
// re-registration only resets the health failure count.
func (r *Registry) Register(ctx context.Context, req httpkit.Registration) error {
	reg, err := r.validate(req)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.regs[req.ServiceName]; ok && sameRegistration(cur.req, req) {
		cur.failures = 0
		return nil
	}
	for name, other := range r.regs {
		if name == req.ServiceName {
			continue
		}
		for _, o := range other.routes {
			for _, n := range reg.routes {
				if o.Prefix == n.Prefix {
					return ycerrors.Conflict(fmt.Sprintf("Prefix %s is registered by %s", n.Prefix, name))
				}
			}
		}
	}

	next := maps.Clone(r.regs)
	next[req.ServiceName] = reg
	table, err := r.build(next)
	if err != nil {
		return ycerrors.Validation().Field("routes", "invalid", err.Error()).Build()
	}
	err = r.publish(ctx, events.TypePlatformServiceRegisteredV1, events.PlatformServiceRegisteredV1{
		ServiceName:  req.ServiceName,
		InstanceID:   req.InstanceID,
		BaseURL:      req.BaseURL,
		Version:      optional(req.Version),
		RegisteredAt: r.now().UTC(),
	})
	if err != nil {
		return err
	}
	r.regs = next
	r.table.Store(table)
	telemetry.LoggerFromContext(ctx).Info("service registered",
		"registered_service", req.ServiceName, "instance_id", req.InstanceID,
		"base_url", req.BaseURL, "routes", len(reg.routes))
	return nil
}

// Deregister removes the service's routes if instanceID still owns them.
func (r *Registry) Deregister(ctx context.Context, serviceName, instanceID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.regs[serviceName]
	if !ok || cur.req.InstanceID != instanceID {
		return ycerrors.NotFound("Registration", serviceName+"/"+instanceID)
	}
	next := maps.Clone(r.regs)
	delete(next, serviceName)
	table, err := r.build(next)
	if err != nil {
		return err
	}
	err = r.publish(ctx, events.TypePlatformServiceDeregisteredV1, events.PlatformServiceDeregisteredV1{
		ServiceName:    serviceName,
		InstanceID:     instanceID,
		Reason:         reason,
		DeregisteredAt: r.now().UTC(),
	})
	if err != nil {
		return err
	}
	r.regs = next
	r.table.Store(table)
	telemetry.LoggerFromContext(ctx).Info("service deregistered",
		"registered_service", serviceName, "instance_id", instanceID, "reason", reason)
	return nil
}

// Poll checks every registration's health URL each HealthInterval until
// ctx ends, deregistering after FailureThreshold consecutive failures.
func (r *Registry) Poll(ctx context.Context) {
	t := time.NewTicker(r.cfg.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.checkAll(ctx)
		}
	}
}

func (r *Registry) checkAll(ctx context.Context) {
	r.mu.Lock()
	type target struct{ service, instance, url string }
	targets := make([]target, 0, len(r.regs))
	for name, reg := range r.regs {
		targets = append(targets, target{name, reg.req.InstanceID, reg.health})
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]bool, len(targets))
	for i, tg := range targets {
		wg.Go(func() { results[i] = r.healthy(ctx, tg.url) })
	}
	wg.Wait()

	for i, tg := range targets {
		if r.recordHealth(tg.service, tg.instance, results[i]) {
			err := r.Deregister(ctx, tg.service, tg.instance, events.PlatformServiceDeregisteredV1ReasonHealthCheckFailed)
			if err != nil && !ycerrors.HasCode(err, ycerrors.CodeNotFound) && ctx.Err() == nil {
				telemetry.LoggerFromContext(ctx).Warn("deregister unhealthy service failed",
					"registered_service", tg.service, "err", err)
			}
		}
	}
}

// recordHealth updates the failure count and reports whether the
// registration has now failed FailureThreshold times in a row.
func (r *Registry) recordHealth(service, instance string, ok bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, found := r.regs[service]
	if !found || reg.req.InstanceID != instance {
		return false
	}
	if ok {
		reg.failures = 0
		return false
	}
	reg.failures++
	return reg.failures >= r.cfg.FailureThreshold
}

func (r *Registry) healthy(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (r *Registry) validate(req httpkit.Registration) (*registration, error) {
	v := ycerrors.Validation()
	if !serviceNameRe.MatchString(req.ServiceName) {
		v.Field("service_name", "invalid", "Must look like yc-name")
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		v.Field("instance_id", "required", "Instance ID is required")
	}
	base, err := url.Parse(req.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		v.Field("base_url", "invalid", "Must be an absolute http(s) URL")
	}
	if !strings.HasPrefix(req.HealthURL, "/") {
		v.Field("health_url", "invalid", "Must be a path under base_url")
	}
	if len(req.Routes) == 0 {
		v.Field("routes", "required", "At least one route is required")
	}
	if v.HasErrors() {
		return nil, v.Build()
	}

	routes := make([]domain.Route, 0, len(req.Routes))
	seen := map[string]bool{}
	for i, rr := range req.Routes {
		field := fmt.Sprintf("routes[%d].prefix", i)
		if r.prefixes[rr.Prefix] {
			v.Field(field, "conflict", "Prefix is a static route")
		}
		if seen[rr.Prefix] {
			v.Field(field, "duplicate", "Prefix appears twice")
		}
		seen[rr.Prefix] = true
		roles := make([]authctx.ActorType, len(rr.Roles))
		for j, role := range rr.Roles {
			roles[j] = authctx.ActorType(role)
		}
		routes = append(routes, domain.Route{
			Prefix: rr.Prefix, Methods: rr.Methods, Upstream: base, Roles: roles,
			Anonymous: rr.Anonymous, Service: req.ServiceName,
		})
	}
	if v.HasErrors() {
		return nil, v.Build()
	}
	if _, err := domain.NewRoutes(routes); err != nil {
		return nil, ycerrors.Validation().Field("routes", "invalid", err.Error()).Build()
	}
	return &registration{
		req:    req,
		routes: routes,
		health: strings.TrimSuffix(base.String(), "/") + req.HealthURL,
	}, nil
}

func (r *Registry) build(regs map[string]*registration) (*domain.Routes, error) {
	all := slices.Clone(r.static)
	for _, reg := range regs {
		all = append(all, reg.routes...)
	}
	return domain.NewRoutes(all)
}

func (r *Registry) publish(ctx context.Context, eventType string, payload any) error {
	return dbkit.InTx(ctx, r.db, func(tx pgx.Tx) error {
		return r.bus.Publish(ctx, tx, eventbus.Event{
			EventType: eventType,
			TenantID:  eventbus.PlatformTenantID,
			Actor:     eventbus.Actor{Type: string(authctx.ActorService), ID: "yc-gateway"},
			Payload:   payload,
		})
	})
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sameRegistration(a, b httpkit.Registration) bool {
	if a.InstanceID != b.InstanceID || a.BaseURL != b.BaseURL || a.Version != b.Version ||
		a.HealthURL != b.HealthURL || len(a.Routes) != len(b.Routes) {
		return false
	}
	for i := range a.Routes {
		x, y := a.Routes[i], b.Routes[i]
		if x.Prefix != y.Prefix || x.Anonymous != y.Anonymous ||
			!slices.Equal(x.Methods, y.Methods) || !slices.Equal(x.Roles, y.Roles) {
			return false
		}
	}
	return true
}
