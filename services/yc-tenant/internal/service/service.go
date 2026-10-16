// Package service holds yc-tenant's use cases: the tenant registry and its
// lifecycle (ADR-029). Every change publishes its event in the same
// transaction (ADR-006).
package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/repository"
)

// MaxPageSize caps a listing.
const MaxPageSize = 100

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Repository is the tenant store; the production implementation is
// repository.Tenants.
type Repository interface {
	Insert(ctx context.Context, tx pgx.Tx, t domain.Tenant) error
	Get(ctx context.Context, tx pgx.Tx, tenantID string, lock bool) (domain.Tenant, bool, error)
	List(ctx context.Context, tx pgx.Tx, f domain.ListFilter, after *repository.Cursor, limit int) ([]domain.Tenant, error)
	HostnameOwners(ctx context.Context, tx pgx.Tx, hostnames []string) (map[string]string, error)
	UpdateDetails(ctx context.Context, tx pgx.Tx, tenantID, name, contactEmail string, at time.Time) error
	ReplaceHostnames(ctx context.Context, tx pgx.Tx, tenantID string, hs []domain.Hostname, at time.Time) error
	SetStatus(ctx context.Context, tx pgx.Tx, t domain.Tenant) error
}

// TxRunner runs fn in a platform transaction (dbkit.WithPlatformTx).
type TxRunner func(ctx context.Context, fn dbkit.TxFunc) error

// Config tunes the service.
type Config struct {
	// PlatformHosts resolve to _platform at yc-gateway; no tenant may use
	// one (ADR-029 rule 3).
	PlatformHosts []string
}

// Service implements the tenant registry use cases.
type Service struct {
	run           TxRunner
	repo          Repository
	bus           Publisher
	platformHosts map[string]bool
	now           func() time.Time
}

// New returns a Service on the database.
func New(db *pgxpool.Pool, bus Publisher, cfg Config) *Service {
	run := func(ctx context.Context, fn dbkit.TxFunc) error { return dbkit.WithPlatformTx(ctx, db, fn) }
	return NewWith(run, repository.Tenants{}, bus, cfg)
}

// NewWith returns a Service on any transaction runner and repository.
func NewWith(run TxRunner, repo Repository, bus Publisher, cfg Config) *Service {
	hosts := make(map[string]bool, len(cfg.PlatformHosts))
	for _, h := range cfg.PlatformHosts {
		if n := domain.NormalizeHostname(h); n != "" {
			hosts[n] = true
		}
	}
	return &Service{run: run, repo: repo, bus: bus, platformHosts: hosts, now: time.Now}
}

// Page is a page of results and the cursor after it.
type Page[T, C any] struct {
	Items []T
	Next  *C
}

// Create registers a shop and announces it (provisioned.v1, hostnames[0]
// primary). A taken tenant ID or hostname is 409.
func (s *Service) Create(ctx context.Context, in domain.CreateInput) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	in, err = in.Validate()
	if err != nil {
		return domain.Tenant{}, err
	}
	if err := s.checkPlatformHosts(in.Hostnames); err != nil {
		return domain.Tenant{}, err
	}
	now := s.now().UTC()
	t := domain.Tenant{
		TenantID: in.TenantID, Name: in.Name, Status: domain.StatusActive, StoreCurrency: in.StoreCurrency,
		DefaultJurisdiction: in.DefaultJurisdiction, IsTaxInclusive: in.IsTaxInclusive, ContactEmail: in.ContactEmail,
		Hostnames: domain.OrderHostnames(in.Hostnames, in.Hostnames[0]), CreatedAt: now, UpdatedAt: now,
	}
	err = s.run(ctx, func(tx pgx.Tx) error {
		if _, ok, err := s.repo.Get(ctx, tx, t.TenantID, false); err != nil {
			return err
		} else if ok {
			return ycerrors.Conflict("A tenant with this ID already exists")
		}
		if err := s.checkHostnamesFree(ctx, tx, t.TenantID, in.Hostnames); err != nil {
			return err
		}
		if err := s.repo.Insert(ctx, tx, t); err != nil {
			return conflict(err)
		}
		var adminEmail *string
		if in.PrimaryAdminEmail != "" {
			adminEmail = &in.PrimaryAdminEmail
		}
		return s.publish(ctx, tx, actor, events.PlatformTenantProvisionedV1{
			TenantID: t.TenantID, Name: t.Name, Hostnames: t.HostnameNames(), StoreCurrency: t.StoreCurrency,
			DefaultJurisdiction: t.DefaultJurisdiction, IsTaxInclusive: t.IsTaxInclusive,
			PrimaryAdminEmail: adminEmail, ProvisionedAt: now,
		})
	})
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

// Get returns a tenant.
func (s *Service) Get(ctx context.Context, tenantID string) (domain.Tenant, error) {
	if _, err := requireSystemAdmin(ctx); err != nil {
		return domain.Tenant{}, err
	}
	var t domain.Tenant
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = s.load(ctx, tx, tenantID, false)
		return err
	})
	return t, err
}

// List returns a page of tenants ordered by tenant_id; limit is the page
// size. Next is set when more follow.
func (s *Service) List(ctx context.Context, f domain.ListFilter, after *repository.Cursor, limit int) (Page[domain.Tenant, repository.Cursor], error) {
	if _, err := requireSystemAdmin(ctx); err != nil {
		return Page[domain.Tenant, repository.Cursor]{}, err
	}
	f, err := f.Validate()
	if err != nil {
		return Page[domain.Tenant, repository.Cursor]{}, err
	}
	limit = min(max(limit, 1), MaxPageSize)
	var items []domain.Tenant
	err = s.run(ctx, func(tx pgx.Tx) error {
		var err error
		items, err = s.repo.List(ctx, tx, f, after, limit+1)
		return err
	})
	if err != nil {
		return Page[domain.Tenant, repository.Cursor]{}, err
	}
	p := Page[domain.Tenant, repository.Cursor]{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		p.Next = &repository.Cursor{TenantID: items[limit-1].TenantID}
	}
	return p, nil
}

// Patch changes the name and contact email. A changed name publishes
// updated.v1; the contact email is not part of any event.
func (s *Service) Patch(ctx context.Context, tenantID string, in domain.PatchInput) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	in, err = in.Validate()
	if err != nil {
		return domain.Tenant{}, err
	}
	var t domain.Tenant
	err = s.run(ctx, func(tx pgx.Tx) error {
		var err error
		if t, err = s.load(ctx, tx, tenantID, true); err != nil {
			return err
		}
		if err := mutable(t); err != nil {
			return err
		}
		nameChanged := in.Name != nil && *in.Name != t.Name
		emailChanged := in.ContactEmail != nil && *in.ContactEmail != t.ContactEmail
		if !nameChanged && !emailChanged {
			return nil
		}
		if nameChanged {
			t.Name = *in.Name
		}
		if emailChanged {
			t.ContactEmail = *in.ContactEmail
		}
		t.UpdatedAt = s.now().UTC()
		if err := s.repo.UpdateDetails(ctx, tx, t.TenantID, t.Name, t.ContactEmail, t.UpdatedAt); err != nil {
			return err
		}
		if !nameChanged {
			return nil
		}
		return s.publishUpdated(ctx, tx, actor, t)
	})
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

// ReplaceHostnames replaces the tenant's hostnames and primary and
// publishes updated.v1 when they changed. A hostname held by another
// tenant or a platform host is 409.
func (s *Service) ReplaceHostnames(ctx context.Context, tenantID string, in domain.HostnamesInput) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	hosts, err := in.Validate()
	if err != nil {
		return domain.Tenant{}, err
	}
	names := domain.HostnameNames(hosts)
	if err := s.checkPlatformHosts(names); err != nil {
		return domain.Tenant{}, err
	}
	var t domain.Tenant
	err = s.run(ctx, func(tx pgx.Tx) error {
		var err error
		if t, err = s.load(ctx, tx, tenantID, true); err != nil {
			return err
		}
		if err := mutable(t); err != nil {
			return err
		}
		if slices.Equal(t.Hostnames, hosts) {
			return nil
		}
		if err := s.checkHostnamesFree(ctx, tx, t.TenantID, names); err != nil {
			return err
		}
		t.UpdatedAt = s.now().UTC()
		if err := s.repo.ReplaceHostnames(ctx, tx, t.TenantID, hosts, t.UpdatedAt); err != nil {
			return conflict(err)
		}
		t.Hostnames = hosts
		return s.publishUpdated(ctx, tx, actor, t)
	})
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

// Suspend moves an active tenant to suspended. The reason is required.
func (s *Service) Suspend(ctx context.Context, tenantID, reason string) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	if reason, err = domain.ValidateReason(reason); err != nil {
		return domain.Tenant{}, err
	}
	return s.transition(ctx, tenantID, func(t *domain.Tenant, now time.Time) (interface{ EventType() string }, error) {
		if t.Status != domain.StatusActive {
			return nil, ycerrors.Conflict("Only an active tenant can be suspended").
				WithInternal("tenant %s is %s", t.TenantID, t.Status)
		}
		t.Status, t.SuspendedAt, t.SuspendedReason = domain.StatusSuspended, &now, reason
		return events.PlatformTenantSuspendedV1{TenantID: t.TenantID, Reason: reason, SuspendedAt: now}, nil
	}, actor)
}

// Reinstate moves a suspended tenant back to active.
func (s *Service) Reinstate(ctx context.Context, tenantID string) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	return s.transition(ctx, tenantID, func(t *domain.Tenant, now time.Time) (interface{ EventType() string }, error) {
		if t.Status != domain.StatusSuspended {
			return nil, ycerrors.Conflict("Only a suspended tenant can be reinstated").
				WithInternal("tenant %s is %s", t.TenantID, t.Status)
		}
		t.Status, t.SuspendedAt, t.SuspendedReason = domain.StatusActive, nil, ""
		return events.PlatformTenantReinstatedV1{TenantID: t.TenantID, ReinstatedAt: now}, nil
	}, actor)
}

// Delete moves a suspended tenant to deleted and frees its hostnames.
// confirmTenantID must repeat the tenant's ID.
func (s *Service) Delete(ctx context.Context, tenantID, confirmTenantID string) (domain.Tenant, error) {
	actor, err := requireSystemAdmin(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	if confirmTenantID != tenantID {
		return domain.Tenant{}, ycerrors.Validation().
			Field("confirm_tenant_id", "mismatch", "confirm_tenant_id must repeat the tenant ID").Build()
	}
	return s.transition(ctx, tenantID, func(t *domain.Tenant, now time.Time) (interface{ EventType() string }, error) {
		if t.Status != domain.StatusSuspended {
			return nil, ycerrors.Conflict("Only a suspended tenant can be deleted").
				WithInternal("tenant %s is %s", t.TenantID, t.Status)
		}
		t.Status, t.DeletedAt, t.Hostnames = domain.StatusDeleted, &now, nil
		return events.PlatformTenantDeletedV1{TenantID: t.TenantID, DeletedAt: now}, nil
	}, actor)
}

// transition applies a lifecycle change under a row lock, stores it and
// publishes the event apply returns.
func (s *Service) transition(
	ctx context.Context, tenantID string,
	apply func(t *domain.Tenant, now time.Time) (interface{ EventType() string }, error),
	actor eventbus.Actor,
) (domain.Tenant, error) {
	var t domain.Tenant
	err := s.run(ctx, func(tx pgx.Tx) error {
		var err error
		if t, err = s.load(ctx, tx, tenantID, true); err != nil {
			return err
		}
		now := s.now().UTC()
		evt, err := apply(&t, now)
		if err != nil {
			return err
		}
		t.UpdatedAt = now
		if err := s.repo.SetStatus(ctx, tx, t); err != nil {
			return err
		}
		return s.publish(ctx, tx, actor, evt)
	})
	if err != nil {
		return domain.Tenant{}, err
	}
	return t, nil
}

func (s *Service) load(ctx context.Context, tx pgx.Tx, tenantID string, lock bool) (domain.Tenant, error) {
	t, ok, err := s.repo.Get(ctx, tx, tenantID, lock)
	if err != nil {
		return domain.Tenant{}, err
	}
	if !ok {
		return domain.Tenant{}, ycerrors.NotFound("tenant", tenantID)
	}
	return t, nil
}

// mutable refuses changes to a deleted tenant.
func mutable(t domain.Tenant) error {
	if t.Status == domain.StatusDeleted {
		return ycerrors.Conflict("A deleted tenant cannot be changed").WithInternal("tenant %s is deleted", t.TenantID)
	}
	return nil
}

// checkPlatformHosts refuses a platform host (ADR-029 rule 3).
func (s *Service) checkPlatformHosts(hostnames []string) error {
	for _, h := range hostnames {
		if s.platformHosts[h] {
			return ycerrors.Conflict("Hostname " + h + " is a platform host")
		}
	}
	return nil
}

// checkHostnamesFree refuses a hostname held by a tenant other than
// tenantID.
func (s *Service) checkHostnamesFree(ctx context.Context, tx pgx.Tx, tenantID string, hostnames []string) error {
	owners, err := s.repo.HostnameOwners(ctx, tx, hostnames)
	if err != nil {
		return err
	}
	for _, h := range hostnames {
		if owner, taken := owners[h]; taken && owner != tenantID {
			return ycerrors.Conflict("Hostname " + h + " is already used by another tenant")
		}
	}
	return nil
}

func conflict(err error) error {
	if errors.Is(err, repository.ErrConflict) {
		return ycerrors.Conflict("The tenant ID or a hostname is already in use").Wrap(err)
	}
	return err
}

func (s *Service) publishUpdated(ctx context.Context, tx pgx.Tx, actor eventbus.Actor, t domain.Tenant) error {
	return s.publish(ctx, tx, actor, events.PlatformTenantUpdatedV1{
		TenantID: t.TenantID, Name: t.Name, Hostnames: t.HostnameNames(), PrimaryHostname: t.PrimaryHostname(),
		UpdatedAt: t.UpdatedAt,
	})
}

// publish enqueues a lifecycle event on _platform, acted by the system
// admin.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, actor eventbus.Actor, payload interface{ EventType() string }) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: payload.EventType(), TenantID: eventbus.PlatformTenantID, Actor: actor, Payload: payload,
	})
}

// requireSystemAdmin refuses other callers and returns the actor for the
// events.
func requireSystemAdmin(ctx context.Context) (eventbus.Actor, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin || a.ActorID == "" {
		return eventbus.Actor{}, ycerrors.Forbidden("System admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}, nil
}
