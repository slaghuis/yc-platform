package domain

import (
	"cmp"
	"fmt"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Request limits.
const (
	MaxLines    = 100
	MaxQuantity = 10_000
)

// Reasons a requested SKU has no price.
const (
	ReasonNoBasePrice     = "no_base_price"
	ReasonUnknownSKU      = "unknown_sku"
	ReasonUnknownTaxClass = "unknown_tax_class"
)

// LineRequest asks for Quantity of a SKU.
type LineRequest struct {
	SKUID    string
	Quantity int64
}

// Request is one evaluation. Jurisdiction defaults to the shop's;
// DisplayCurrency, when it differs from the store currency, adds a
// converted view (ADR-014).
type Request struct {
	Lines           []LineRequest
	Jurisdiction    string
	DisplayCurrency string
}

// Validate checks the request's shape.
func (r Request) Validate() error {
	v := ycerrors.Validation()
	if n := len(r.Lines); n == 0 || n > MaxLines {
		v.Field("lines", "invalid", fmt.Sprintf("Must have 1-%d lines", MaxLines))
	}
	seen := make(map[string]bool, len(r.Lines))
	for i, l := range r.Lines {
		if l.Quantity < 1 || l.Quantity > MaxQuantity {
			v.Field(fmt.Sprintf("lines[%d].quantity", i), "invalid", fmt.Sprintf("Must be 1-%d", MaxQuantity))
		}
		if seen[l.SKUID] {
			v.Field(fmt.Sprintf("lines[%d].sku_id", i), "duplicate", "Each SKU may appear once")
		}
		seen[l.SKUID] = true
	}
	if r.Jurisdiction != "" && !jurisdictionPattern.MatchString(r.Jurisdiction) {
		v.Field("jurisdiction", "invalid", "Must be an ISO 3166 country or subdivision code, e.g. ZA")
	}
	if r.DisplayCurrency != "" && !money.ValidCurrency(r.DisplayCurrency) {
		v.Field("display_currency", "invalid", "Must be an ISO 4217 code")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// SKUIDs returns the requested SKUs.
func (r Request) SKUIDs() []string {
	ids := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		ids[i] = l.SKUID
	}
	return ids
}

// Inputs is the data an evaluation reads, loaded by the caller.
type Inputs struct {
	Config     Config
	Prices     map[string]BasePrice // by SKU
	TaxClasses map[string]string    // SKU → tax class ID, from yc-product events
	Classes    map[string]TaxClass  // by class ID
	Rate       *ExchangeRate        // store → display currency, when requested
}

// Line is a priced line. Net + Tax = Gross, all in the store currency.
type Line struct {
	SKUID            string
	Quantity         int64
	TaxClass         string
	UnitPrice        money.Money
	PriceIncludesTax bool
	Net              money.Money
	Tax              money.Money
	Gross            money.Money
	Taxes            []TaxAmount
}

// Unpriced is a requested SKU that could not be priced.
type Unpriced struct {
	SKUID  string
	Reason string
}

// TaxTotal is one row of the VAT breakdown.
type TaxTotal struct {
	Rule   string
	Rate   money.Rate
	Amount money.Money
}

// Totals sum the priced lines.
type Totals struct {
	SubtotalExclTax money.Money
	TaxTotal        money.Money
	GrandTotal      money.Money
	TaxBreakdown    []TaxTotal
}

// Display is the cosmetic conversion to the visitor's currency. Amounts
// are converted one by one, so they need not sum exactly.
type Display struct {
	Currency      string
	Rate          money.Rate
	Provider      string
	RateFetchedAt time.Time
	Lines         []DisplayLine
	Totals        DisplayTotals
}

// DisplayLine is a line in the display currency.
type DisplayLine struct {
	SKUID     string
	UnitPrice money.Money
	Net       money.Money
	Tax       money.Money
	Gross     money.Money
}

// DisplayTotals are the totals in the display currency.
type DisplayTotals struct {
	SubtotalExclTax money.Money
	TaxTotal        money.Money
	GrandTotal      money.Money
}

// AuditEntry records what one evaluator did.
type AuditEntry struct {
	Evaluator string
	Detail    map[string]any
}

// Result is an evaluation's outcome. TaxInclusive is the shop's display
// preference: show Gross when true, Net otherwise.
type Result struct {
	StoreCurrency string
	TaxInclusive  bool
	Jurisdiction  string
	Lines         []Line
	Unpriced      []Unpriced
	Totals        Totals
	Display       *Display
	Audit         []AuditEntry
	EvaluatedAt   time.Time
}

// Evaluator is one pipeline stage.
type Evaluator interface {
	Name() string
	Evaluate(req Request, in *Inputs, res *Result) error
}

// Pipeline runs evaluators in order.
type Pipeline []Evaluator

// DefaultPipeline is the Phase 2 pipeline. Promotion evaluators slot in
// between BasePrice and Tax in Phase 3.
func DefaultPipeline() Pipeline {
	return Pipeline{BasePriceEvaluator{}, TaxEvaluator{}, DisplayCurrencyEvaluator{}}
}

// Run evaluates req against in.
func (p Pipeline) Run(req Request, in *Inputs, now time.Time) (Result, error) {
	res := Result{
		StoreCurrency: in.Config.StoreCurrency,
		TaxInclusive:  in.Config.TaxInclusive,
		Jurisdiction:  cmp.Or(req.Jurisdiction, in.Config.DefaultJurisdiction),
		EvaluatedAt:   now,
	}
	for _, e := range p {
		if err := e.Evaluate(req, in, &res); err != nil {
			return Result{}, fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return res, nil
}

// BasePriceEvaluator starts each line from the SKU's base price.
type BasePriceEvaluator struct{}

// Name implements Evaluator.
func (BasePriceEvaluator) Name() string { return "base_price" }

// Evaluate implements Evaluator.
func (e BasePriceEvaluator) Evaluate(req Request, in *Inputs, res *Result) error {
	audit := make([]map[string]any, 0, len(req.Lines))
	for _, l := range req.Lines {
		price, ok := in.Prices[l.SKUID]
		if !ok {
			res.Unpriced = append(res.Unpriced, Unpriced{SKUID: l.SKUID, Reason: ReasonNoBasePrice})
			continue
		}
		class, ok := in.TaxClasses[l.SKUID]
		if !ok {
			res.Unpriced = append(res.Unpriced, Unpriced{SKUID: l.SKUID, Reason: ReasonUnknownSKU})
			continue
		}
		amount, err := price.Price.Times(l.Quantity)
		if err != nil {
			return err
		}
		line := Line{
			SKUID: l.SKUID, Quantity: l.Quantity, TaxClass: class,
			UnitPrice: price.Price, PriceIncludesTax: price.TaxInclusive,
			Net: amount, Gross: amount,
		}
		res.Lines = append(res.Lines, line)
		audit = append(audit, map[string]any{
			"sku_id": l.SKUID, "unit_price": price.Price, "quantity": l.Quantity,
			"line_amount": amount, "price_includes_tax": price.TaxInclusive, "valid_from": price.ValidFrom.UTC(),
		})
	}
	res.Audit = append(res.Audit, AuditEntry{Evaluator: e.Name(), Detail: map[string]any{"lines": audit}})
	return nil
}

// TaxEvaluator levies each line's tax class rules for the jurisdiction
// (ADR-016), then totals the lines.
type TaxEvaluator struct{}

// Name implements Evaluator.
func (TaxEvaluator) Name() string { return "tax" }

// Evaluate implements Evaluator.
func (e TaxEvaluator) Evaluate(_ Request, in *Inputs, res *Result) error {
	priced := res.Lines[:0]
	audit := make([]map[string]any, 0, len(res.Lines))
	for _, line := range res.Lines {
		class, ok := in.Classes[line.TaxClass]
		if !ok {
			res.Unpriced = append(res.Unpriced, Unpriced{SKUID: line.SKUID, Reason: ReasonUnknownTaxClass})
			continue
		}
		rules := class.RulesFor(res.Jurisdiction)
		var err error
		line.Net, line.Gross, line.Taxes, err = ApplyTaxes(line.Gross, line.PriceIncludesTax, rules)
		if err != nil {
			return err
		}
		if line.Tax, err = line.Gross.Sub(line.Net); err != nil {
			return err
		}
		priced = append(priced, line)

		entry := map[string]any{
			"sku_id": line.SKUID, "tax_class": line.TaxClass,
			"mode": "added", "net": line.Net, "tax": line.Tax, "gross": line.Gross,
		}
		if line.PriceIncludesTax {
			entry["mode"] = "extracted"
		}
		if len(rules) == 0 {
			entry["note"] = "no rules for " + res.Jurisdiction + ": untaxed"
		}
		applied := make([]map[string]any, len(line.Taxes))
		for i, t := range line.Taxes {
			applied[i] = map[string]any{
				"jurisdiction": t.Jurisdiction, "rule": t.Rule, "rate": t.Rate.String(),
				"base": t.Base, "base_amount": t.BaseAmount, "amount": t.Amount,
			}
		}
		entry["rules"] = applied
		audit = append(audit, entry)
	}
	res.Lines = priced
	res.Audit = append(res.Audit, AuditEntry{Evaluator: e.Name(), Detail: map[string]any{
		"jurisdiction": res.Jurisdiction, "lines": audit,
	}})
	return res.total()
}

func (res *Result) total() error {
	cur := res.StoreCurrency
	t := Totals{SubtotalExclTax: money.Zero(cur), TaxTotal: money.Zero(cur), GrandTotal: money.Zero(cur)}
	index := map[string]int{}
	for _, l := range res.Lines {
		var err error
		if t.SubtotalExclTax, err = t.SubtotalExclTax.Add(l.Net); err != nil {
			return err
		}
		if t.TaxTotal, err = t.TaxTotal.Add(l.Tax); err != nil {
			return err
		}
		if t.GrandTotal, err = t.GrandTotal.Add(l.Gross); err != nil {
			return err
		}
		for _, tax := range l.Taxes {
			key := tax.Rule + "@" + tax.Rate.String()
			i, ok := index[key]
			if !ok {
				i = len(t.TaxBreakdown)
				index[key] = i
				t.TaxBreakdown = append(t.TaxBreakdown, TaxTotal{Rule: tax.Rule, Rate: tax.Rate, Amount: money.Zero(cur)})
			}
			if t.TaxBreakdown[i].Amount, err = t.TaxBreakdown[i].Amount.Add(tax.Amount); err != nil {
				return err
			}
		}
	}
	res.Totals = t
	return nil
}

// DisplayCurrencyEvaluator converts the result for display. It runs last
// and never changes the store-currency amounts (ADR-014).
type DisplayCurrencyEvaluator struct{}

// Name implements Evaluator.
func (DisplayCurrencyEvaluator) Name() string { return "display_currency" }

// Evaluate implements Evaluator.
func (e DisplayCurrencyEvaluator) Evaluate(req Request, in *Inputs, res *Result) error {
	to := req.DisplayCurrency
	if to == "" || to == res.StoreCurrency {
		return nil
	}
	if in.Rate == nil {
		return ycerrors.Validation().Field("display_currency", "unavailable", "No exchange rate to "+to).Build()
	}
	r := in.Rate.Rate
	conv := func(m money.Money, err error) (money.Money, error) {
		if err != nil {
			return money.Money{}, err
		}
		return m.Convert(to, r)
	}
	d := &Display{Currency: to, Rate: r, Provider: in.Rate.Provider, RateFetchedAt: in.Rate.FetchedAt, Lines: make([]DisplayLine, len(res.Lines))}
	var err error
	for i, l := range res.Lines {
		dl := DisplayLine{SKUID: l.SKUID}
		dl.UnitPrice, err = conv(l.UnitPrice, err)
		dl.Net, err = conv(l.Net, err)
		dl.Tax, err = conv(l.Tax, err)
		dl.Gross, err = conv(l.Gross, err)
		d.Lines[i] = dl
	}
	d.Totals.SubtotalExclTax, err = conv(res.Totals.SubtotalExclTax, err)
	d.Totals.TaxTotal, err = conv(res.Totals.TaxTotal, err)
	d.Totals.GrandTotal, err = conv(res.Totals.GrandTotal, err)
	if err != nil {
		return err
	}
	res.Display = d
	res.Audit = append(res.Audit, AuditEntry{Evaluator: e.Name(), Detail: map[string]any{
		"from": res.StoreCurrency, "to": to, "rate": r.String(),
		"provider": in.Rate.Provider, "rate_fetched_at": in.Rate.FetchedAt.UTC(),
	}})
	return nil
}
