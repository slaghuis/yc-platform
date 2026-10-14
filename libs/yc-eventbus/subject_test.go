package eventbus_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

func TestSubjectForEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tenant  string
		event   string
		want    string
		wantErr bool
	}{
		{"aggregate root", "shop-abc", "order.placed.v1", "yc.shop-abc.order.placed.v1", false},
		{"with entity", "shop-abc", "inventory.stock.reserved.v1", "yc.shop-abc.inventory.stock.reserved.v1", false},
		{"platform", "_platform", "platform.tenant.provisioned.v1", "yc._platform.platform.tenant.provisioned.v1", false},
		{"empty tenant", "", "order.placed.v1", "", true},
		{"dotted tenant", "shop.abc", "order.placed.v1", "", true},
		{"wildcard tenant", "*", "order.placed.v1", "", true},
		{"tail wildcard tenant", ">", "order.placed.v1", "", true},
		{"uppercase tenant", "Shop-ABC", "order.placed.v1", "", true},
		{"underscore tenant", "shop_abc", "order.placed.v1", "", true},
		{"spoofed platform tenant", "_platform2", "order.placed.v1", "", true},
		{"short event", "shop-abc", "order.v1", "", true},
		{"too long", "shop-abc", "order.line.item.placed.v1", "", true},
		{"repeated domain", "shop-abc", "order.order.placed.v1", "", true},
		{"no version", "shop-abc", "order.placed", "", true},
		{"bad version", "shop-abc", "order.placed.vX", "", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := eventbus.SubjectForEvent(tt.tenant, tt.event)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSubjectForDLQ(t *testing.T) {
	t.Parallel()
	s, err := eventbus.SubjectForDLQ("shop-abc", "order")
	require.NoError(t, err)
	assert.Equal(t, "yc.shop-abc.order.dlq.v1", s)

	_, err = eventbus.SubjectForDLQ("shop.abc", "order")
	require.Error(t, err)
}

func TestValidateTenantID(t *testing.T) {
	t.Parallel()
	valid := []string{"_platform", "shop-abc", "abc", "shop1", "a-b-c", strings.Repeat("a", 63)}
	for _, id := range valid {
		assert.NoError(t, eventbus.ValidateTenantID(id), id)
	}
	invalid := []string{
		"", "shop.abc", "*", ">", "shop abc", "Shop", "shop_abc",
		"_shop", "-shop", "shop-", "shop--abc", strings.Repeat("a", 64),
	}
	for _, id := range invalid {
		assert.Error(t, eventbus.ValidateTenantID(id), id)
	}
}

func TestTenantOfSubject(t *testing.T) {
	t.Parallel()
	got, err := eventbus.TenantOfSubject("yc.shop-abc.order.placed.v1")
	require.NoError(t, err)
	assert.Equal(t, "shop-abc", got)

	got, err = eventbus.TenantOfSubject("yc._platform.platform.tenant.provisioned.v1")
	require.NoError(t, err)
	assert.Equal(t, "_platform", got)

	for _, s := range []string{"", "yc", "yc.shop-abc", "zz.shop-abc.order.placed.v1", "yc.*.order.placed.v1"} {
		_, err := eventbus.TenantOfSubject(s)
		assert.Error(t, err, s)
	}
}

func TestCheckSubjectTenant(t *testing.T) {
	t.Parallel()
	require.NoError(t, eventbus.CheckSubjectTenant("yc.shop-abc.order.placed.v1", "shop-abc"))

	err := eventbus.CheckSubjectTenant("yc.shop-abc.order.placed.v1", "shop-xyz")
	require.Error(t, err)
	assert.True(t, eventbus.IsPoison(err))

	err = eventbus.CheckSubjectTenant("yc.shop-abc.order.placed.v1", "")
	require.Error(t, err)
	assert.True(t, eventbus.IsPoison(err))

	err = eventbus.CheckSubjectTenant("not-a-yc-subject", "shop-abc")
	require.Error(t, err)
	assert.True(t, eventbus.IsPoison(err))
}

func TestDomainOfEventType(t *testing.T) {
	t.Parallel()
	d, err := eventbus.DomainOfEventType("order.placed.v1")
	require.NoError(t, err)
	assert.Equal(t, "order", d)
}
