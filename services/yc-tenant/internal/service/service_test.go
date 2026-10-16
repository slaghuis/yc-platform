package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

func TestLifecycle(t *testing.T) {
	h := newHarness()
	ctx := sysadmin()

	in := validCreate()
	in.PrimaryAdminEmail = "owner@acme.test"
	created, err := h.svc.Create(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, created.Status)
	require.Equal(t, []string{events.TypePlatformTenantProvisionedV1}, h.bus.types())
	var prov events.PlatformTenantProvisionedV1
	h.bus.payload(t, 0, &prov)
	assert.Equal(t, []string{"acme.localhost", "www.acme.localhost"}, prov.Hostnames)
	require.NotNil(t, prov.PrimaryAdminEmail)
	assert.Equal(t, "owner@acme.test", *prov.PrimaryAdminEmail)
	assert.Equal(t, eventbus.PlatformTenantID, h.bus.events[0].TenantID)
	assert.Equal(t, "admin-1", h.bus.events[0].Actor.ID)

	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Create(ctx, domain.CreateInput{
			TenantID: "other", Name: "Other", Hostnames: []string{"acme.localhost"},
			StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
		})
		return err
	})), "hostname taken")
	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Create(ctx, domain.CreateInput{
			TenantID: "plat", Name: "Plat", Hostnames: []string{"admin.localhost"},
			StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
		})
		return err
	})), "platform host")
	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Create(ctx, domain.CreateInput{
			TenantID: "plat2", Name: "Plat", Hostnames: []string{"console.example.com"},
			StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
		})
		return err
	})), "platform host normalised")

	email := "new@acme.test"
	patched, err := h.svc.Patch(ctx, "acme", domain.PatchInput{ContactEmail: &email})
	require.NoError(t, err)
	assert.Equal(t, "new@acme.test", patched.ContactEmail)
	assert.Len(t, h.bus.types(), 1, "contact_email-only publishes nothing")

	name := "Acme Shop"
	patched, err = h.svc.Patch(ctx, "acme", domain.PatchInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, "Acme Shop", patched.Name)
	assert.Equal(t, events.TypePlatformTenantUpdatedV1, h.bus.types()[1])

	replaced, err := h.svc.ReplaceHostnames(ctx, "acme", domain.HostnamesInput{
		Hostnames: []string{"www.acme.localhost", "acme.localhost"}, Primary: "www.acme.localhost",
	})
	require.NoError(t, err)
	assert.Equal(t, "www.acme.localhost", replaced.PrimaryHostname())
	assert.Equal(t, events.TypePlatformTenantUpdatedV1, h.bus.types()[2])

	suspended, err := h.svc.Suspend(ctx, "acme", "non-payment")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSuspended, suspended.Status)
	assert.Equal(t, events.TypePlatformTenantSuspendedV1, h.bus.types()[3])
	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Suspend(ctx, "acme", "again")
		return err
	})))

	active, err := h.svc.Reinstate(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, active.Status)
	assert.Equal(t, events.TypePlatformTenantReinstatedV1, h.bus.types()[4])

	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Delete(ctx, "acme", "acme")
		return err
	})), "must be suspended")

	_, err = h.svc.Suspend(ctx, "acme", "closing")
	require.NoError(t, err)
	assert.Equal(t, 400, statusOf(mustErr(t, func() error {
		_, err := h.svc.Delete(ctx, "acme", "wrong")
		return err
	})))

	deleted, err := h.svc.Delete(ctx, "acme", "acme")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusDeleted, deleted.Status)
	assert.Empty(t, deleted.Hostnames)
	assert.Equal(t, events.TypePlatformTenantDeletedV1, h.bus.types()[len(h.bus.types())-1])
	assert.Equal(t, 409, statusOf(mustErr(t, func() error {
		_, err := h.svc.Patch(ctx, "acme", domain.PatchInput{Name: &name})
		return err
	})), "deleted is immutable")

	// Freed hostname can be reused.
	_, err = h.svc.Create(ctx, domain.CreateInput{
		TenantID: "acme-2", Name: "Acme 2", Hostnames: []string{"acme.localhost"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"acme.localhost"}, sortedHosts(h.repo))
}

func TestListAndAuth(t *testing.T) {
	h := newHarness()
	h.seed(t, "alpha", "alpha.test")
	h.seed(t, "beta", "beta.test")
	h.seed(t, "gamma", "gamma.test")
	_, err := h.svc.Suspend(sysadmin(), "beta", "pause")
	require.NoError(t, err)
	h.bus.events = nil

	page, err := h.svc.List(sysadmin(), domain.ListFilter{Status: domain.StatusActive}, nil, 1)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "alpha", page.Items[0].TenantID)
	require.NotNil(t, page.Next)

	page, err = h.svc.List(sysadmin(), domain.ListFilter{Status: domain.StatusActive}, page.Next, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "gamma", page.Items[0].TenantID)
	assert.Nil(t, page.Next)

	assert.Equal(t, 403, statusOf(mustErr(t, func() error {
		_, err := h.svc.Get(shopAdmin(), "alpha")
		return err
	})))
}

func mustErr(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	require.Error(t, err)
	return err
}
