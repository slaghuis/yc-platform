package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

func TestMarketingTopicTransactional(t *testing.T) {
	for _, typ := range domain.Types {
		assert.Empty(t, typ.MarketingTopic(), string(typ))
	}
}

func TestAllowsMarketing(t *testing.T) {
	assert.True(t, domain.AllowsMarketing("newsletter", true, false, false))
	assert.True(t, domain.AllowsMarketing("marketing", false, true, false))
	assert.True(t, domain.AllowsMarketing("new_products", false, false, true))
	assert.False(t, domain.AllowsMarketing("newsletter", false, true, true))
	assert.False(t, domain.AllowsMarketing("unknown", true, true, true))
}
