package service

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/repository"
)

// fakeRepo is an in-memory Repository.
type fakeRepo struct {
	tenants map[string]domain.Tenant
	// hosts maps a hostname to its tenant.
	hosts map[string]string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{tenants: map[string]domain.Tenant{}, hosts: map[string]string{}}
}

var _ Repository = (*fakeRepo)(nil)

func (r *fakeRepo) Insert(_ context.Context, _ pgx.Tx, t domain.Tenant) error {
	if _, ok := r.tenants[t.TenantID]; ok {
		return repository.ErrConflict
	}
	for _, h := range t.Hostnames {
		if _, ok := r.hosts[h.Name]; ok {
			return repository.ErrConflict
		}
	}
	r.tenants[t.TenantID] = t
	for _, h := range t.Hostnames {
		r.hosts[h.Name] = t.TenantID
	}
	return nil
}

func (r *fakeRepo) Get(_ context.Context, _ pgx.Tx, id string, _ bool) (domain.Tenant, bool, error) {
	t, ok := r.tenants[id]
	t.Hostnames = slices.Clone(t.Hostnames)
	return t, ok, nil
}

func (r *fakeRepo) List(_ context.Context, _ pgx.Tx, f domain.ListFilter, after *repository.Cursor, limit int) ([]domain.Tenant, error) {
	ids := slices.Sorted(maps.Keys(r.tenants))
	var out []domain.Tenant
	for _, id := range ids {
		t := r.tenants[id]
		if after != nil && id <= after.TenantID {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.Search != "" && !matches(t, f.Search) {
			continue
		}
		out = append(out, t)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func matches(t domain.Tenant, q string) bool {
	q = strings.ToLower(q)
	if strings.Contains(strings.ToLower(t.Name), q) {
		return true
	}
	for _, h := range t.Hostnames {
		if strings.Contains(h.Name, q) {
			return true
		}
	}
	return false
}

func (r *fakeRepo) HostnameOwners(_ context.Context, _ pgx.Tx, hostnames []string) (map[string]string, error) {
	out := map[string]string{}
	for _, h := range hostnames {
		if id, ok := r.hosts[h]; ok {
			out[h] = id
		}
	}
	return out, nil
}

func (r *fakeRepo) UpdateDetails(_ context.Context, _ pgx.Tx, id, name, email string, at time.Time) error {
	t := r.tenants[id]
	t.Name, t.ContactEmail, t.UpdatedAt = name, email, at
	r.tenants[id] = t
	return nil
}

func (r *fakeRepo) ReplaceHostnames(_ context.Context, _ pgx.Tx, id string, hs []domain.Hostname, at time.Time) error {
	for h, owner := range r.hosts {
		if owner == id {
			delete(r.hosts, h)
		}
	}
	for _, h := range hs {
		if _, ok := r.hosts[h.Name]; ok {
			return repository.ErrConflict
		}
		r.hosts[h.Name] = id
	}
	t := r.tenants[id]
	t.Hostnames, t.UpdatedAt = hs, at
	r.tenants[id] = t
	return nil
}

func (r *fakeRepo) SetStatus(_ context.Context, _ pgx.Tx, t domain.Tenant) error {
	stored := r.tenants[t.TenantID]
	stored.Status, stored.UpdatedAt = t.Status, t.UpdatedAt
	stored.SuspendedAt, stored.SuspendedReason, stored.DeletedAt = t.SuspendedAt, t.SuspendedReason, t.DeletedAt
	if t.Status == domain.StatusDeleted {
		for h, owner := range r.hosts {
			if owner == t.TenantID {
				delete(r.hosts, h)
			}
		}
		stored.Hostnames = nil
	}
	r.tenants[t.TenantID] = stored
	return nil
}

// fakeBus records published events.
type fakeBus struct {
	events []eventbus.Event
	err    error
}

func (b *fakeBus) Publish(_ context.Context, _ pgx.Tx, evt eventbus.Event) error {
	if b.err != nil {
		return b.err
	}
	b.events = append(b.events, evt)
	return nil
}

// types returns the published event types in order.
func (b *fakeBus) types() []string {
	out := make([]string, len(b.events))
	for i, e := range b.events {
		out[i] = e.EventType
	}
	return out
}

// payload decodes the i-th event's payload into v.
func (b *fakeBus) payload(t *testing.T, i int, v any) {
	t.Helper()
	raw, err := json.Marshal(b.events[i].Payload)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}

type harness struct {
	svc  *Service
	repo *fakeRepo
	bus  *fakeBus
	now  time.Time
}

// newHarness returns a Service on fakes, with admin.localhost as the
// platform host and a clock that moves one minute per reading.
func newHarness() *harness {
	h := &harness{repo: newFakeRepo(), bus: &fakeBus{}, now: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	run := func(ctx context.Context, fn dbkit.TxFunc) error { return fn(nil) }
	h.svc = NewWith(run, h.repo, h.bus, Config{PlatformHosts: []string{"admin.localhost", " Console.Example.com "}})
	h.svc.now = func() time.Time {
		h.now = h.now.Add(time.Minute)
		return h.now
	}
	return h
}

func sysadmin() context.Context {
	return authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: eventbus.PlatformTenantID, ActorID: "admin-1", ActorType: authctx.ActorSystemAdmin,
	})
}

func shopAdmin() context.Context {
	return authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "acme", ActorID: "user-1", ActorType: authctx.ActorShopAdmin,
	})
}

func validCreate() domain.CreateInput {
	return domain.CreateInput{
		TenantID: "acme", Name: "Acme", Hostnames: []string{"acme.localhost", "www.acme.localhost"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true, ContactEmail: "owner@acme.test",
	}
}

// seed creates a tenant and drops the events it published.
func (h *harness) seed(t *testing.T, id string, hosts ...string) {
	t.Helper()
	in := validCreate()
	in.TenantID = id
	in.Hostnames = hosts
	_, err := h.svc.Create(sysadmin(), in)
	require.NoError(t, err)
	h.bus.events = nil
}

// statusOf returns the HTTP status of err, 0 for nil.
func statusOf(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := ycerrors.As(err); ok {
		return e.Status
	}
	return -1
}

func sortedHosts(r *fakeRepo) []string {
	out := slices.Collect(maps.Keys(r.hosts))
	sort.Strings(out)
	return out
}
