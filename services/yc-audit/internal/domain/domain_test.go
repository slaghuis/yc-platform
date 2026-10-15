package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
)

func TestRules(t *testing.T) {
	subject, summary := domain.Rules[events.TypePaymentChargebackReceivedV1].Describe(map[string]any{
		"transaction_id": "t1", "gateway": "stripe", "reason": "fraudulent",
		"amount": map[string]any{"amount_minor": float64(12345), "currency": "ZAR"},
	})
	assert.Equal(t, "transaction:t1", subject)
	assert.Equal(t, "Chargeback of ZAR 123.45 on a stripe payment (fraudulent)", summary)

	_, summary = domain.Rules[events.TypeIdentityUserDeletedV1].Describe(map[string]any{
		"user_id": "u1", "role": "customer", "requested_by": "system_admin",
	})
	assert.Equal(t, "Customer account deleted at the system admin's request", summary)

	for _, typ := range domain.AuditedTypes() {
		assert.True(t, domain.Rules[typ].Category.Valid(), typ)
	}
	assert.NotContains(t, domain.ShopCategories(), domain.CategoryGDPR)
}

func TestRegister(t *testing.T) {
	now := time.Now()
	r := domain.Register{
		Expected: []string{"yc-identity", "yc-cart"}, Status: domain.RegisterInProgress, Deadline: now.Add(time.Hour),
		Reports: []domain.Report{{Service: "yc-identity"}, {Service: "yc-other"}},
	}
	assert.Equal(t, []string{"yc-cart"}, r.Missing())
	assert.False(t, r.Completes())
	assert.False(t, r.Overdue(now))
	assert.True(t, r.Overdue(now.Add(2*time.Hour)))

	r.Reports = append(r.Reports, domain.Report{Service: "yc-cart"})
	assert.True(t, r.Completes())
	assert.False(t, r.Overdue(now.Add(2*time.Hour)))

	r.Status = domain.RegisterIncident
	assert.True(t, r.Completes(), "late reports still complete an incident")
	r.Status = domain.RegisterComplete
	assert.False(t, r.Completes())
}
