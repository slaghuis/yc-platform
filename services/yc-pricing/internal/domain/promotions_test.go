package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

func TestSalePriceEvaluator(t *testing.T) {
	const sku = "sku-sale"
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	in := &domain.Inputs{
		Config: domain.Config{StoreCurrency: "ZAR", TaxInclusive: true, DefaultJurisdiction: "ZA"},
		Prices: map[string]domain.BasePrice{
			sku: {SKUID: sku, Price: zar(20000), TaxInclusive: true},
		},
		TaxClasses: map[string]string{sku: "za-standard"},
		Classes: map[string]domain.TaxClass{
			"za-standard": {ID: "za-standard", Rules: []domain.TaxRule{rule("ZA", "VAT", "0.15", domain.BaseLineTotal, 0)}},
		},
		Sales: map[string]domain.SalePrice{
			sku: {SKUID: sku, Price: zar(15000), TaxInclusive: true, Name: "Autumn"},
		},
	}
	req := domain.Request{Lines: []domain.LineRequest{{SKUID: sku, Quantity: 2}}}

	res, err := domain.DefaultPipeline().Run(req, in, now)
	require.NoError(t, err)
	require.Len(t, res.Lines, 1)
	line := res.Lines[0]
	assert.True(t, line.HasSale)
	assert.Equal(t, zar(15000), line.UnitPrice)
	require.NotNil(t, line.ListUnitPrice)
	assert.Equal(t, zar(20000), *line.ListUnitPrice)
	assert.Equal(t, zar(30000), line.Gross)
	require.Len(t, line.Discounts, 1)
	assert.Equal(t, domain.DiscountTypeSalePrice, line.Discounts[0].Type)
	assert.Equal(t, "Autumn", line.Discounts[0].Name)
	assert.Equal(t, zar(10000), line.Discounts[0].Amount, "2 × (20000−15000)")
}

func TestBuyXGetYEvaluator(t *testing.T) {
	const (
		qualSKU = "sku-qual"
		tgtSKU  = "sku-tgt"
		qualPID = "prod-qual"
		tgtPID  = "prod-tgt"
	)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	base := func() *domain.Inputs {
		return &domain.Inputs{
			Config: domain.Config{StoreCurrency: "ZAR", TaxInclusive: false, DefaultJurisdiction: "ZA"},
			Prices: map[string]domain.BasePrice{
				qualSKU: {SKUID: qualSKU, Price: zar(10000), TaxInclusive: false},
				tgtSKU:  {SKUID: tgtSKU, Price: zar(8000), TaxInclusive: false},
			},
			TaxClasses: map[string]string{qualSKU: "za-zero", tgtSKU: "za-zero"},
			ProductIDs: map[string]string{qualSKU: qualPID, tgtSKU: tgtPID},
			Classes: map[string]domain.TaxClass{
				"za-zero": {ID: "za-zero", Rules: []domain.TaxRule{rule("ZA", "VAT", "0", domain.BaseLineTotal, 0)}},
			},
		}
	}
	req := domain.Request{Lines: []domain.LineRequest{
		{SKUID: qualSKU, Quantity: 1},
		{SKUID: tgtSKU, Quantity: 1},
	}}

	t.Run("percent discount", func(t *testing.T) {
		in := base()
		in.Campaigns = []domain.BXGYCampaign{{
			ID: "c1", Name: "Half off target", QualifyingSKUID: qualSKU, TargetSKUID: tgtSKU,
			DiscountType: domain.DiscountKindPercent, DiscountValue: 5000, Enabled: true, StartsAt: now.Add(-time.Hour),
		}}
		res, err := domain.DefaultPipeline().Run(req, in, now)
		require.NoError(t, err)
		require.Len(t, res.Lines, 2)
		tgt := res.Lines[1]
		assert.False(t, tgt.HasSale)
		assert.Equal(t, zar(4000), tgt.Gross)
		require.Len(t, tgt.Discounts, 1)
		assert.Equal(t, domain.DiscountTypeBXGY, tgt.Discounts[0].Type)
		assert.Equal(t, zar(4000), tgt.Discounts[0].Amount)
	})

	t.Run("fixed discount", func(t *testing.T) {
		in := base()
		in.Campaigns = []domain.BXGYCampaign{{
			ID: "c2", Name: "R20 off", QualifyingProductID: qualPID, TargetProductID: tgtPID,
			DiscountType: domain.DiscountKindFixed, DiscountValue: 2000, Enabled: true, StartsAt: now.Add(-time.Hour),
		}}
		res, err := domain.DefaultPipeline().Run(req, in, now)
		require.NoError(t, err)
		tgt := res.Lines[1]
		assert.Equal(t, zar(6000), tgt.Gross)
		require.Len(t, tgt.Discounts, 1)
		assert.Equal(t, zar(2000), tgt.Discounts[0].Amount)
	})

	t.Run("skips target with sale", func(t *testing.T) {
		in := base()
		in.Sales = map[string]domain.SalePrice{
			tgtSKU: {SKUID: tgtSKU, Price: zar(5000), TaxInclusive: false, Name: "Flash"},
		}
		in.Campaigns = []domain.BXGYCampaign{{
			ID: "c3", Name: "BXGY", QualifyingSKUID: qualSKU, TargetSKUID: tgtSKU,
			DiscountType: domain.DiscountKindPercent, DiscountValue: 5000, Enabled: true, StartsAt: now.Add(-time.Hour),
		}}
		res, err := domain.DefaultPipeline().Run(req, in, now)
		require.NoError(t, err)
		tgt := res.Lines[1]
		assert.True(t, tgt.HasSale)
		assert.Equal(t, zar(5000), tgt.Gross)
		require.Len(t, tgt.Discounts, 1)
		assert.Equal(t, domain.DiscountTypeSalePrice, tgt.Discounts[0].Type)
	})
}

func TestBXGYDiscountAmount(t *testing.T) {
	c := domain.BXGYCampaign{DiscountType: domain.DiscountKindPercent, DiscountValue: 3333}
	off, err := c.DiscountAmount(zar(10000))
	require.NoError(t, err)
	assert.Equal(t, zar(3333), off)

	c = domain.BXGYCampaign{DiscountType: domain.DiscountKindFixed, DiscountValue: 15000}
	off, err = c.DiscountAmount(zar(10000))
	require.NoError(t, err)
	assert.Equal(t, zar(10000), off, "capped at line gross")
}

func TestSalePriceInputValidate(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	assert.NoError(t, domain.SalePriceInput{Price: zar(100), StartsAt: start, EndsAt: &end}.Validate("ZAR"))
	assert.Error(t, domain.SalePriceInput{Price: money.New(100, "USD"), StartsAt: start}.Validate("ZAR"))
}
