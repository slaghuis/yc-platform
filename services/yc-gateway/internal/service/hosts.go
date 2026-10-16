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

// HostState is the outcome of resolving a request Host.
type HostState int

const (
	// HostUnknown means no mapping and not a deleted hostname.
	HostUnknown HostState = iota
	// HostActive means mapped to a live shop.
	HostActive
	// HostSuspended means mapped, but the shop is suspended (503).
	HostSuspended
	// HostDeleted means formerly mapped; answers 410 until reclaimed.
	HostDeleted
)

// Hosts resolves request hostnames to tenants from memory (ADR-020 rule 1).
// The static map from config is loaded first; tenant_hosts rows add to it.
// Suspended tenants and deleted hostnames are also kept in memory and in
// yc_gateway so a restart keeps 503/410 answers (ADR-029).
type Hosts struct {
	db        *pgxpool.Pool
	repo      repository.TenantHosts
	suspended repository.SuspendedTenants
	static    map[string]string
	current   atomic.Pointer[map[string]string]
	suspend   atomic.Pointer[map[string]string] // tenant_id → reason
	deleted   atomic.Pointer[map[string]string] // hostname → former tenant
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
	empty := map[string]string{}
	h.suspend.Store(&empty)
	h.deleted.Store(&empty)
	return h, nil
}

// Resolve returns the tenant for a Host header value (legacy callers).
func (h *Hosts) Resolve(host string) (string, bool) {
	tenant, state := h.Lookup(host)
	return tenant, state == HostActive || state == HostSuspended
}

// Lookup reports the tenant and lifecycle state for a Host header.
func (h *Hosts) Lookup(host string) (string, HostState) {
	norm := NormalizeHost(host)
	if former, ok := (*h.deleted.Load())[norm]; ok {
		return former, HostDeleted
	}
	tenant, ok := (*h.current.Load())[norm]
	if !ok {
		return "", HostUnknown
	}
	if _, suspended := (*h.suspend.Load())[tenant]; suspended {
		return tenant, HostSuspended
	}
	return tenant, HostActive
}

// Load merges tenant_hosts, suspended_tenants and deleted_hosts into memory.
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

	susp, err := h.suspended.All(ctx, h.db)
	if err != nil {
		return err
	}
	h.suspend.Store(&susp)

	del, err := h.repo.AllDeleted(ctx, h.db)
	if err != nil {
		return err
	}
	h.deleted.Store(&del)
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
	normalised, err := h.normaliseHosts(tenantID, hostnames)
	if err != nil {
		return err
	}

	err = dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
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

	h.addHosts(tenantID, normalised)
	h.clearDeleted(normalised)
	telemetry.LoggerFromContext(ctx).Info("tenant hostnames mapped",
		"provisioned_tenant", tenantID, "hostnames", normalised)
	return nil
}

// ReplaceHostnames swaps a tenant's hostname set (platform.tenant.updated.v1).
func (h *Hosts) ReplaceHostnames(ctx context.Context, tenantID string, hostnames []string) error {
	normalised, err := h.normaliseHosts(tenantID, hostnames)
	if err != nil {
		return err
	}

	err = dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
		if err := h.repo.DropTenant(ctx, tx, tenantID); err != nil {
			return err
		}
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

	h.removeTenantHosts(tenantID)
	h.addHosts(tenantID, normalised)
	h.clearDeleted(normalised)
	telemetry.LoggerFromContext(ctx).Info("tenant hostnames replaced",
		"tenant_id", tenantID, "hostnames", normalised)
	return nil
}

// Suspend records a shop as suspended (503 on every route including webhooks).
func (h *Hosts) Suspend(ctx context.Context, tenantID, reason string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	err := dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
		return h.suspended.Put(ctx, tx, tenantID, reason)
	})
	if err != nil {
		return err
	}
	for {
		old := h.suspend.Load()
		next := maps.Clone(*old)
		next[tenantID] = reason
		if h.suspend.CompareAndSwap(old, &next) {
			break
		}
	}
	telemetry.LoggerFromContext(ctx).Info("tenant suspended", "tenant_id", tenantID)
	return nil
}

// Reinstate clears a suspension.
func (h *Hosts) Reinstate(ctx context.Context, tenantID string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	err := dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
		return h.suspended.Delete(ctx, tx, tenantID)
	})
	if err != nil {
		return err
	}
	for {
		old := h.suspend.Load()
		next := maps.Clone(*old)
		delete(next, tenantID)
		if h.suspend.CompareAndSwap(old, &next) {
			break
		}
	}
	telemetry.LoggerFromContext(ctx).Info("tenant reinstated", "tenant_id", tenantID)
	return nil
}

// Delete frees hostnames and marks them gone (410) until reclaimed.
func (h *Hosts) Delete(ctx context.Context, tenantID string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	var hosts []string
	err := dbkit.InTx(ctx, h.db, func(tx pgx.Tx) error {
		var err error
		hosts, err = h.repo.HostsOf(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if err := h.repo.DropTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := h.repo.MarkDeleted(ctx, tx, tenantID, hosts); err != nil {
			return err
		}
		return h.suspended.Delete(ctx, tx, tenantID)
	})
	if err != nil {
		return err
	}

	h.removeTenantHosts(tenantID)
	for {
		old := h.suspend.Load()
		next := maps.Clone(*old)
		delete(next, tenantID)
		if h.suspend.CompareAndSwap(old, &next) {
			break
		}
	}
	for {
		old := h.deleted.Load()
		next := maps.Clone(*old)
		for _, host := range hosts {
			next[host] = tenantID
		}
		if h.deleted.CompareAndSwap(old, &next) {
			break
		}
	}
	telemetry.LoggerFromContext(ctx).Info("tenant deleted", "tenant_id", tenantID, "hostnames", hosts)
	return nil
}

func (h *Hosts) normaliseHosts(tenantID string, hostnames []string) ([]string, error) {
	if err := validateShopTenant(tenantID); err != nil {
		return nil, fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	normalised := make([]string, 0, len(hostnames))
	for _, host := range hostnames {
		norm := NormalizeHost(host)
		if norm == "" {
			return nil, fmt.Errorf("%w: invalid hostname %q", eventbus.ErrPoison, host)
		}
		if s, ok := h.static[norm]; ok && s != tenantID {
			return nil, fmt.Errorf("%w: hostname %s is %s in config", eventbus.ErrPoison, norm, s)
		}
		normalised = append(normalised, norm)
	}
	return normalised, nil
}

func (h *Hosts) addHosts(tenantID string, hosts []string) {
	for {
		old := h.current.Load()
		next := maps.Clone(*old)
		for _, host := range hosts {
			next[host] = tenantID
		}
		if h.current.CompareAndSwap(old, &next) {
			return
		}
	}
}

func (h *Hosts) removeTenantHosts(tenantID string) {
	for {
		old := h.current.Load()
		next := maps.Clone(*old)
		for host, t := range next {
			if t == tenantID {
				if _, static := h.static[host]; !static {
					delete(next, host)
				}
			}
		}
		if h.current.CompareAndSwap(old, &next) {
			return
		}
	}
}

func (h *Hosts) clearDeleted(hosts []string) {
	for {
		old := h.deleted.Load()
		next := maps.Clone(*old)
		for _, host := range hosts {
			delete(next, host)
		}
		if h.deleted.CompareAndSwap(old, &next) {
			return
		}
	}
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
