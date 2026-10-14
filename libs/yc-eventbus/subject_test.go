package eventbus_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/<org>/yc-platform/libs/yc-eventbus"
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
		{"standard", "shop-abc", "order.order.placed.v1", "yc.shop-abc.order.order.placed.v1", false},
		{"platform", "_platform", "tenant.tenant.provisioned.v1", "yc._platform.tenant.tenant.provisioned.v1", false},
		{"empty tenant", "", "order.order.placed.v1", "", true},
		{"short event", "shop-abc", "order.v1", "", true},
		{"no version", "shop-abc", "order.order.placed", "", true},
		{"bad version", "shop-abc", "order.order.placed.vX", "", true},
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
}

func TestDomainOfEventType(t *testing.T) {
	t.Parallel()
	d, err := eventbus.DomainOfEventType("order.order.placed.v1")
	require.NoError(t, err)
	assert.Equal(t, "order", d)
}