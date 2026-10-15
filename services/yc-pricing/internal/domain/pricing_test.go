package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

func zar(minor int64) money.Money { return money.New(minor, "ZAR") }

func rule(j, name, rate, base string, order int) domain.TaxRule {
	return domain.TaxRule{Jurisdiction: j, Name: name, Rate: money.MustRate(rate), Base: base, SortOrder: order}
}

func amounts(taxes []domain.TaxAmount) []int64 {
	out := make([]int64, len(taxes))
	for i, t := range taxes {
		out[i] = t.Amount.AmountMinor
	}
	return out
}

func TestApplyTaxes(t *testing.T) {
	vat := []domain.TaxRule{rule("ZA", "VAT", "0.15", domain.BaseLineTotal, 0)}
	compound := []domain.TaxRule{
		rule("XX", "A", "0.1", domain.BaseLineTotal, 0),
		rule("XX", "B", "0.05", domain.BaseIncludingPrefix+"A", 1),
	}
	split := []domain.TaxRule{
		rule("XX", "A", "0.075", domain.BaseLineTotal, 0),
		rule("XX", "B", "0.075", domain.BaseLineTotal, 1),
	}

	tests := []struct {
		name       string
		amount     int64
		inclusive  bool
		rules      []domain.TaxRule
		net, gross int64
		taxes      []int64
	}{
		{"VAT extracted", 20000, true, vat, 17391, 20000, []int64{2609}},
		{"VAT added", 17391, false, vat, 17391, 20000, []int64{2609}},
		{"VAT extracted from a round price", 11500, true, vat, 10000, 11500, []int64{1500}},
		{"compound added", 10000, false, compound, 10000, 11550, []int64{1000, 550}},
		{"compound extracted", 11550, true, compound, 10000, 11550, []int64{1000, 550}},
		{"last rule absorbs rounding", 1001, true, split, 870, 1001, []int64{65, 66}},
		{"zero-rated", 5000, true, []domain.TaxRule{rule("ZA", "VAT", "0", domain.BaseLineTotal, 0)}, 5000, 5000, []int64{0}},
		{"untaxed", 5000, true, nil, 5000, 5000, []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			net, gross, taxes, err := domain.ApplyTaxes(zar(tt.amount), tt.inclusive, tt.rules)
			require.NoError(t, err)
			assert.Equal(t, zar(tt.net), net)
			assert.Equal(t, zar(tt.gross), gross)
			assert.Equal(t, tt.taxes, amounts(taxes))
		})
	}
}

func fieldCodes(t *testing.T, err error) []string {
	t.Helper()
	e, ok := ycerrors.As(err)
	require.True(t, ok, "%v", err)
	codes := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		codes[i] = f.Field + ":" + f.Code
	}
	return codes
}

func TestTaxClassInputValidate(t *testing.T) {
	r := func(j, name, rate, base string, order int) domain.TaxRuleInput {
		return domain.TaxRuleInput{Jurisdiction: j, Name: name, Rate: rate, Base: base, SortOrder: order}
	}

	rules, err := domain.TaxClassInput{DisplayName: "Standard", Rules: []domain.TaxRuleInput{
		r("CA-QC", "QST", "0.09975", domain.BaseIncludingPrefix+"GST", 2),
		r("CA-QC", "GST", "0.05", domain.BaseLineTotal, 1),
		r("ZA", "VAT", "0.15", domain.BaseLineTotal, 0),
	}}.Validate()
	require.NoError(t, err)
	require.Len(t, rules, 3)
	assert.Equal(t, []string{"GST", "QST", "VAT"}, []string{rules[0].Name, rules[1].Name, rules[2].Name})

	_, err = domain.TaxClassInput{DisplayName: "", Rules: []domain.TaxRuleInput{
		r("za", "VAT", "15%", domain.BaseLineTotal, 0),
	}}.Validate()
	assert.Equal(t, []string{"display_name:invalid", "rules[0].jurisdiction:invalid", "rules[0].rate:invalid"}, fieldCodes(t, err))

	_, err = domain.TaxClassInput{DisplayName: "X", Rules: []domain.TaxRuleInput{
		r("ZA", "A", "0.1", domain.BaseIncludingPrefix+"B", 0),
		r("ZA", "B", "0.1", domain.BaseLineTotal, 1),
		r("ZA", "B", "0.1", "price", 1),
	}}.Validate()
	assert.Equal(t, []string{"rules:invalid_base", "rules:duplicate", "rules:ambiguous_order", "rules:invalid_base"}, fieldCodes(t, err))

	assert.Error(t, domain.ValidateClassID("ZA Standard"))
	assert.NoError(t, domain.ValidateClassID("za-standard"))
}

func TestRulesFor(t *testing.T) {
	c := domain.TaxClass{Rules: []domain.TaxRule{
		rule("CA", "GST", "0.05", domain.BaseLineTotal, 0),
		rule("CA-QC", "GST", "0.05", domain.BaseLineTotal, 0),
		rule("CA-QC", "QST", "0.09975", domain.BaseLineTotal, 1),
	}}
	assert.Len(t, c.RulesFor("CA-QC"), 2)
	assert.Len(t, c.RulesFor("CA-ON"), 1, "falls back to the country")
	assert.Empty(t, c.RulesFor("ZA"))
}

func TestPipeline(t *testing.T) {
	const (
		shirt = "sku-shirt"
		book  = "sku-book"
		bare  = "sku-no-price"
		ghost = "sku-unsynced"
		odd   = "sku-odd-class"
	)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	in := &domain.Inputs{
		Config: domain.Config{StoreCurrency: "ZAR", TaxInclusive: true, DefaultJurisdiction: "ZA"},
		Prices: map[string]domain.BasePrice{
			shirt: {SKUID: shirt, Price: zar(20000), TaxInclusive: true},
			book:  {SKUID: book, Price: zar(10000), TaxInclusive: false},
			ghost: {SKUID: ghost, Price: zar(100), TaxInclusive: true},
			odd:   {SKUID: odd, Price: zar(100), TaxInclusive: true},
		},
		TaxClasses: map[string]string{shirt: "za-standard", book: "za-zero", bare: "za-standard", odd: "nope"},
		Classes: map[string]domain.TaxClass{
			"za-standard": {ID: "za-standard", Rules: []domain.TaxRule{rule("ZA", "VAT", "0.15", domain.BaseLineTotal, 0)}},
			"za-zero":     {ID: "za-zero", Rules: []domain.TaxRule{rule("ZA", "VAT", "0", domain.BaseLineTotal, 0)}},
		},
		Rate: &domain.ExchangeRate{Base: "ZAR", Quote: "USD", Rate: money.MustRate("0.055"), Provider: "frankfurter", FetchedAt: now},
	}
	req := domain.Request{
		Lines: []domain.LineRequest{
			{SKUID: shirt, Quantity: 2},
			{SKUID: book, Quantity: 1},
			{SKUID: bare, Quantity: 1},
			{SKUID: ghost, Quantity: 1},
			{SKUID: odd, Quantity: 1},
		},
		DisplayCurrency: "USD",
	}
	require.NoError(t, req.Validate())

	res, err := domain.DefaultPipeline().Run(req, in, now)
	require.NoError(t, err)

	assert.Equal(t, "ZA", res.Jurisdiction)
	require.Len(t, res.Lines, 2)
	assert.Equal(t, zar(34783), res.Lines[0].Net, "2 × R200.00 incl. VAT")
	assert.Equal(t, zar(5217), res.Lines[0].Tax)
	assert.Equal(t, zar(40000), res.Lines[0].Gross)
	assert.Equal(t, zar(10000), res.Lines[1].Gross, "zero-rated, entered excl. tax")
	assert.Equal(t, []domain.Unpriced{
		{SKUID: bare, Reason: domain.ReasonNoBasePrice},
		{SKUID: ghost, Reason: domain.ReasonUnknownSKU},
		{SKUID: odd, Reason: domain.ReasonUnknownTaxClass},
	}, res.Unpriced)

	assert.Equal(t, zar(44783), res.Totals.SubtotalExclTax)
	assert.Equal(t, zar(5217), res.Totals.TaxTotal)
	assert.Equal(t, zar(50000), res.Totals.GrandTotal)
	require.Len(t, res.Totals.TaxBreakdown, 2)
	assert.Equal(t, "VAT", res.Totals.TaxBreakdown[0].Rule)
	assert.Equal(t, "0.15", res.Totals.TaxBreakdown[0].Rate.String())
	assert.Equal(t, zar(5217), res.Totals.TaxBreakdown[0].Amount)
	assert.Equal(t, zar(0), res.Totals.TaxBreakdown[1].Amount)

	require.NotNil(t, res.Display)
	assert.Equal(t, money.New(2750, "USD"), res.Display.Totals.GrandTotal, "R500.00 at 0.055")
	assert.Equal(t, money.New(1100, "USD"), res.Display.Lines[0].UnitPrice)

	evaluators := make([]string, len(res.Audit))
	for i, a := range res.Audit {
		evaluators[i] = a.Evaluator
	}
	assert.Equal(t, []string{"base_price", "tax", "display_currency"}, evaluators)

	t.Run("display currency without a rate", func(t *testing.T) {
		noRate := *in
		noRate.Rate = nil
		_, err := domain.DefaultPipeline().Run(req, &noRate, now)
		assert.Equal(t, []string{"display_currency:unavailable"}, fieldCodes(t, err))
	})

	t.Run("store currency needs no conversion", func(t *testing.T) {
		r := req
		r.DisplayCurrency = "ZAR"
		res, err := domain.DefaultPipeline().Run(r, in, now)
		require.NoError(t, err)
		assert.Nil(t, res.Display)
	})
}

func TestRequestValidate(t *testing.T) {
	err := domain.Request{
		Lines:        []domain.LineRequest{{SKUID: "a", Quantity: 0}, {SKUID: "a", Quantity: 1}},
		Jurisdiction: "za", DisplayCurrency: "usd",
	}.Validate()
	assert.Equal(t, []string{
		"lines[0].quantity:invalid", "lines[1].sku_id:duplicate", "jurisdiction:invalid", "display_currency:invalid",
	}, fieldCodes(t, err))
	assert.Error(t, domain.Request{}.Validate())
}
