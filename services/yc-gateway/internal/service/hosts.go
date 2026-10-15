// Package service holds yc-gateway's use cases.
package service

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/repository"
)

// Hosts resolves request hostnames to tenants from memory (ADR-020 rule 1).
// The static map from config is loaded first; tenant_hosts rows add to it.
type Hosts struct {
	db      *pgxpool.Pool
	repo    repository.TenantHosts
	static  map[string]string
	current atomic.Pointer[map[string]string]
}

// NewHosts validates the static map and the platform hostnames (which
// resolve to _platform, ADR-020 rule 5) and returns a registry holding
// only them; call Load to add the database rows.
func NewHosts(db *pgxpool.Pool, static map[string]string, platform []string) (*Hosts, error) {
	h := &Hosts{db: db, static: map[string]string{}}
	for _, host := range platform {
		norm := NormalizeHost(host)
		if norm == "" {
			return nil, fmt.Errorf("platform_hosts: invalid hostname %q", host)
		}
		h.static[norm] = eventbus.PlatformTenantID
	}
	for host, tenant := range static {
		norm := NormalizeHost(host)
		if norm == "" {
			return nil, fmt.Errorf("tenant_hosts: invalid hostname %q", host)
		}
		if err := validateShopTenant(tenant); err != nil {
			return nil, fmt.Errorf("tenant_hosts: %s: %w", host, err)
		}
		if _, ok := h.static[norm]; ok {
			return nil, fmt.Errorf("tenant_hosts: %s is also a platform host", host)
		}
		h.static[norm] = tenant
	}
	m := maps.Clone(h.static)
	h.current.Store(&m)
	return h, nil
}

// Resolve returns the tenant for a Host header value.
func (h *Hosts) Resolve(host string) (string, bool) {
	tenant, ok := (*h.current.Load())[NormalizeHost(host)]
	return tenant, ok
}

// Load merges tenant_hosts into the static map. A hostname mapped to
// different tenants by config and database is an error (ADR-020 rule 2).
func (h *Hosts) Load(ctx context.Context) error {
	rows, err := h.repo.All(ctx, h.db)
	if err != nil {
		return err
	}
	merged := maps.Clone(h.static)
	for host, tenant := range rows {
		if s, ok := h.static[host]; ok && s != tenant {
			return fmt.Errorf("hostname %s is %s in config but %s in tenant_hosts", host, s, tenant)
		}
		merged[host] = tenant
	}
	h.current.Store(&merged)
	return nil
}

// Refresh reloads tenant_hosts every interval until ctx ends, so instances
// that did not handle a provisioning event still learn its hostnames.
func (h *Hosts) Refresh(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.Load(ctx); err != nil && ctx.Err() == nil {
				telemetry.LoggerFromContext(ctx).Warn("tenant hosts refresh failed", "err", err)
			}
		}
	}
}

// Provision maps hostnames to tenantID, durably and then in memory. A
// hostname that already belongs to another tenant is a poison event: it
// needs a human, not a retry.
func (h *Hosts) Provision(ctx context.Context, tenantID string, hostnames []string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	normalised := make([]string, 0, len(hostnames))
	for _, host := range hostnames {
		norm := NormalizeHost(host)
		if norm == "" {
			return fmt.Errorf("%w: invalid hostname %q", eventbus.ErrPoison, host)
		}
		if s, ok := h.static[norm]; ok && s != tenantID {
			return fmt.Errorf("%w: hostname %s is %s in config", eventbus.ErrPoison, norm, s)
		}
		normalised = append(normalised, norm)
	}

	err := dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
		for _, host := range normalised {
			owner, err := h.repo.Claim(ctx, tx, host, tenantID)
			if err != nil {
				return err
			}
			if owner != tenantID {
				return fmt.Errorf("%w: hostname %s already belongs to %s", eventbus.ErrPoison, host, owner)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	for {
		old := h.current.Load()
		next := maps.Clone(*old)
		for _, host := range normalised {
			next[host] = tenantID
		}
		if h.current.CompareAndSwap(old, &next) {
			break
		}
	}
	telemetry.LoggerFromContext(ctx).Info("tenant hostnames mapped",
		"provisioned_tenant", tenantID, "hostnames", normalised)
	return nil
}

// NormalizeHost lowercases host and strips any port and trailing dot. It
// returns "" for an empty or malformed host.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.ContainsAny(host, "/@ :") {
		return ""
	}
	return host
}

func validateShopTenant(tenantID string) error {
	if tenantID == eventbus.PlatformTenantID {
		return fmt.Errorf("hostnames map to shop tenants, not %s", tenantID)
	}
	return eventbus.ValidateTenantID(tenantID)
}
