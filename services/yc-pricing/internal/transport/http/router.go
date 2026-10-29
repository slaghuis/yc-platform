// Package http is yc-pricing's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"cmp"
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	api "github.com/slaghuis/yc-platform/services/yc-pricing/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

// Pricing is the service the handlers call.
type Pricing interface {
	GetConfig(ctx context.Context) (domain.Config, error)
	PutConfig(ctx context.Context, in domain.ConfigInput) (domain.Config, error)
	SetPrice(ctx context.Context, skuID string, price money.Money) (domain.BasePrice, error)
	RemovePrice(ctx context.Context, skuID string) error
	PriceHistory(ctx context.Context, skuID string) ([]domain.PricePeriod, error)
	ListPrices(ctx context.Context, afterSKU string, limit int) ([]domain.BasePrice, string, error)
	ListTaxClasses(ctx context.Context) ([]domain.TaxClass, error)
	GetTaxClass(ctx context.Context, id string) (domain.TaxClass, error)
	PutTaxClass(ctx context.Context, id string, in domain.TaxClassInput) (domain.TaxClass, bool, error)
	DeleteTaxClass(ctx context.Context, id string) error
	Evaluate(ctx context.Context, req domain.Request) (domain.Result, error)
	SetSalePrice(ctx context.Context, skuID string, in domain.SalePriceInput) (domain.SalePrice, error)
	GetSalePrice(ctx context.Context, skuID string) (domain.SalePrice, error)
	RemoveSalePrice(ctx context.Context, skuID string) error
	CreateBXGY(ctx context.Context, in domain.BXGYInput) (domain.BXGYCampaign, error)
	UpdateBXGY(ctx context.Context, id string, in domain.BXGYInput) (domain.BXGYCampaign, error)
	GetBXGY(ctx context.Context, id string) (domain.BXGYCampaign, error)
	ListBXGY(ctx context.Context) ([]domain.BXGYCampaign, error)
	DeleteBXGY(ctx context.Context, id string) error
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Pricing, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-pricing registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/prices", Methods: []string{http.MethodGet, http.MethodPost}},
		{Prefix: "/admin/prices", Roles: admin},
		{Prefix: "/admin/sale-prices", Roles: admin},
		{Prefix: "/admin/campaigns", Roles: admin},
		{Prefix: "/admin/pricing-config", Roles: admin},
		{Prefix: "/admin/tax-classes", Roles: admin},
		{Prefix: "/internal/prices", Methods: []string{http.MethodPost}},
	}
}

type handlers struct {
	svc Pricing
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) EvaluatePrices(ctx context.Context, req api.EvaluatePricesRequestObject) (api.EvaluatePricesResponseObject, error) {
	res, err := h.evaluate(ctx, *req.Body, deref(req.Params.DisplayCurrency), deref(req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.EvaluatePrices200JSONResponse(res), nil
}

func (h handlers) EvaluatePricesInternal(ctx context.Context, req api.EvaluatePricesInternalRequestObject) (api.EvaluatePricesInternalResponseObject, error) {
	res, err := h.evaluate(ctx, *req.Body, deref(req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.EvaluatePricesInternal200JSONResponse(res), nil
}

// evaluate prices body; the display currency is the body's, else the first
// non-empty fallback.
func (h handlers) evaluate(ctx context.Context, body api.EvaluateRequest, fallbacks ...string) (api.PricingResult, error) {
	r := domain.Request{
		Lines:           make([]domain.LineRequest, len(body.Lines)),
		Jurisdiction:    deref(body.Jurisdiction),
		DisplayCurrency: cmp.Or(append([]string{deref(body.DisplayCurrency)}, fallbacks...)...),
	}
	for i, l := range body.Lines {
		r.Lines[i] = domain.LineRequest{SKUID: l.SkuID.String(), Quantity: l.Quantity}
	}
	res, err := h.svc.Evaluate(ctx, r)
	if err != nil {
		return api.PricingResult{}, err
	}
	return result(res), nil
}

func (h handlers) GetSKUPrice(ctx context.Context, req api.GetSKUPriceRequestObject) (api.GetSKUPriceResponseObject, error) {
	id := req.ID.String()
	res, err := h.svc.Evaluate(ctx, domain.Request{
		Lines:           []domain.LineRequest{{SKUID: id, Quantity: 1}},
		Jurisdiction:    deref(req.Params.Jurisdiction),
		DisplayCurrency: cmp.Or(deref(req.Params.DisplayCurrency), deref(req.Params.XYCDisplayCurrency)),
	})
	if err != nil {
		return nil, err
	}
	if len(res.Lines) == 0 {
		return nil, ycerrors.NotFound("Price", id).WithInternal("unpriced: %s", res.Unpriced[0].Reason)
	}
	return api.GetSKUPrice200JSONResponse(result(res)), nil
}

func (h handlers) ListPrices(ctx context.Context, req api.ListPricesRequestObject) (api.ListPricesResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after string
	if page.Cursor != "" {
		if err := httpkit.DecodeCursor(page.Cursor, &after); err != nil {
			return nil, err
		}
		if _, err := uuid.Parse(after); err != nil {
			return nil, ycerrors.Validation().Field("cursor", "invalid", "Cursor is not valid").Wrap(err)
		}
	}
	prices, next, err := h.svc.ListPrices(ctx, after, page.Size)
	if err != nil {
		return nil, err
	}
	out := api.ListPrices200JSONResponse{Items: make([]api.BasePrice, len(prices)), Page: httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}}
	for i, p := range prices {
		out.Items[i] = basePrice(p)
	}
	if next != "" {
		out.Page.NextCursor = httpkit.EncodeCursor(next)
	}
	return out, nil
}

func (h handlers) GetPriceHistory(ctx context.Context, req api.GetPriceHistoryRequestObject) (api.GetPriceHistoryResponseObject, error) {
	periods, err := h.svc.PriceHistory(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	out := api.GetPriceHistory200JSONResponse{SkuID: req.ID, Items: make([]api.PricePeriod, len(periods))}
	for i, p := range periods {
		out.Items[i] = api.PricePeriod{
			Price: p.Price, TaxInclusive: p.TaxInclusive, ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, SetBy: optional(p.SetBy),
		}
	}
	return out, nil
}

func (h handlers) SetPrice(ctx context.Context, req api.SetPriceRequestObject) (api.SetPriceResponseObject, error) {
	p, err := h.svc.SetPrice(ctx, req.ID.String(), req.Body.Price)
	if err != nil {
		return nil, err
	}
	return api.SetPrice200JSONResponse(basePrice(p)), nil
}

func (h handlers) RemovePrice(ctx context.Context, req api.RemovePriceRequestObject) (api.RemovePriceResponseObject, error) {
	if err := h.svc.RemovePrice(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.RemovePrice204Response{}, nil
}

func (h handlers) GetPricingConfig(ctx context.Context, _ api.GetPricingConfigRequestObject) (api.GetPricingConfigResponseObject, error) {
	c, err := h.svc.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetPricingConfig200JSONResponse(config(c)), nil
}

func (h handlers) PutPricingConfig(ctx context.Context, req api.PutPricingConfigRequestObject) (api.PutPricingConfigResponseObject, error) {
	c, err := h.svc.PutConfig(ctx, domain.ConfigInput{
		StoreCurrency: req.Body.StoreCurrency, TaxInclusive: req.Body.TaxInclusive, DefaultJurisdiction: req.Body.DefaultJurisdiction,
	})
	if err != nil {
		return nil, err
	}
	return api.PutPricingConfig200JSONResponse(config(c)), nil
}

func (h handlers) ListTaxClasses(ctx context.Context, _ api.ListTaxClassesRequestObject) (api.ListTaxClassesResponseObject, error) {
	classes, err := h.svc.ListTaxClasses(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListTaxClasses200JSONResponse{Items: make([]api.TaxClass, len(classes))}
	for i, c := range classes {
		out.Items[i] = taxClass(c)
	}
	return out, nil
}

func (h handlers) GetTaxClass(ctx context.Context, req api.GetTaxClassRequestObject) (api.GetTaxClassResponseObject, error) {
	c, err := h.svc.GetTaxClass(ctx, req.ClassID)
	if err != nil {
		return nil, err
	}
	return api.GetTaxClass200JSONResponse(taxClass(c)), nil
}

func (h handlers) PutTaxClass(ctx context.Context, req api.PutTaxClassRequestObject) (api.PutTaxClassResponseObject, error) {
	in := domain.TaxClassInput{DisplayName: req.Body.DisplayName, Rules: make([]domain.TaxRuleInput, len(req.Body.Rules))}
	for i, r := range req.Body.Rules {
		in.Rules[i] = domain.TaxRuleInput{
			Jurisdiction: r.Jurisdiction, Name: r.RuleName, Rate: r.Rate, Base: r.Base, SortOrder: r.SortOrder,
		}
	}
	c, created, err := h.svc.PutTaxClass(ctx, req.ClassID, in)
	if err != nil {
		return nil, err
	}
	if created {
		return api.PutTaxClass201JSONResponse(taxClass(c)), nil
	}
	return api.PutTaxClass200JSONResponse(taxClass(c)), nil
}

func (h handlers) DeleteTaxClass(ctx context.Context, req api.DeleteTaxClassRequestObject) (api.DeleteTaxClassResponseObject, error) {
	if err := h.svc.DeleteTaxClass(ctx, req.ClassID); err != nil {
		return nil, err
	}
	return api.DeleteTaxClass204Response{}, nil
}

func (h handlers) GetSalePrice(ctx context.Context, req api.GetSalePriceRequestObject) (api.GetSalePriceResponseObject, error) {
	s, err := h.svc.GetSalePrice(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetSalePrice200JSONResponse(salePrice(s)), nil
}

func (h handlers) SetSalePrice(ctx context.Context, req api.SetSalePriceRequestObject) (api.SetSalePriceResponseObject, error) {
	s, err := h.svc.SetSalePrice(ctx, req.ID.String(), domain.SalePriceInput{
		Price: req.Body.Price, Name: deref(req.Body.Name), StartsAt: req.Body.StartsAt, EndsAt: req.Body.EndsAt,
	})
	if err != nil {
		return nil, err
	}
	return api.SetSalePrice200JSONResponse(salePrice(s)), nil
}

func (h handlers) RemoveSalePrice(ctx context.Context, req api.RemoveSalePriceRequestObject) (api.RemoveSalePriceResponseObject, error) {
	if err := h.svc.RemoveSalePrice(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.RemoveSalePrice204Response{}, nil
}

func (h handlers) ListBXGYCampaigns(ctx context.Context, _ api.ListBXGYCampaignsRequestObject) (api.ListBXGYCampaignsResponseObject, error) {
	items, err := h.svc.ListBXGY(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListBXGYCampaigns200JSONResponse{Items: make([]api.BXGYCampaign, len(items))}
	for i, c := range items {
		out.Items[i] = bxgy(c)
	}
	return out, nil
}

func (h handlers) CreateBXGYCampaign(ctx context.Context, req api.CreateBXGYCampaignRequestObject) (api.CreateBXGYCampaignResponseObject, error) {
	c, err := h.svc.CreateBXGY(ctx, bxgyInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateBXGYCampaign201JSONResponse(bxgy(c)), nil
}

func (h handlers) GetBXGYCampaign(ctx context.Context, req api.GetBXGYCampaignRequestObject) (api.GetBXGYCampaignResponseObject, error) {
	c, err := h.svc.GetBXGY(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetBXGYCampaign200JSONResponse(bxgy(c)), nil
}

func (h handlers) UpdateBXGYCampaign(ctx context.Context, req api.UpdateBXGYCampaignRequestObject) (api.UpdateBXGYCampaignResponseObject, error) {
	c, err := h.svc.UpdateBXGY(ctx, req.ID.String(), bxgyInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateBXGYCampaign200JSONResponse(bxgy(c)), nil
}

func (h handlers) DeleteBXGYCampaign(ctx context.Context, req api.DeleteBXGYCampaignRequestObject) (api.DeleteBXGYCampaignResponseObject, error) {
	if err := h.svc.DeleteBXGY(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteBXGYCampaign204Response{}, nil
}

func result(r domain.Result) api.PricingResult {
	out := api.PricingResult{
		StoreCurrency: r.StoreCurrency,
		TaxInclusive:  r.TaxInclusive,
		Jurisdiction:  r.Jurisdiction,
		Lines:         make([]api.PricedLine, len(r.Lines)),
		Unpriced:      make([]api.Unpriced, len(r.Unpriced)),
		Totals: api.Totals{
			SubtotalExclTax: r.Totals.SubtotalExclTax, TaxTotal: r.Totals.TaxTotal, GrandTotal: r.Totals.GrandTotal,
			TaxBreakdown: make([]api.TaxTotal, len(r.Totals.TaxBreakdown)),
		},
		AuditTrail:  make([]api.AuditEntry, len(r.Audit)),
		EvaluatedAt: r.EvaluatedAt.UTC().Truncate(time.Millisecond),
	}
	for i, l := range r.Lines {
		pl := api.PricedLine{
			SkuID: parseUUID(l.SKUID), Quantity: l.Quantity, TaxClass: l.TaxClass,
			UnitPrice: l.UnitPrice, PriceIncludesTax: l.PriceIncludesTax,
			Net: l.Net, Tax: l.Tax, Gross: l.Gross, Taxes: make([]api.LineTax, len(l.Taxes)),
			ListUnitPrice: l.ListUnitPrice,
		}
		for j, t := range l.Taxes {
			pl.Taxes[j] = api.LineTax{
				Jurisdiction: t.Jurisdiction, Rule: t.Rule, Rate: t.Rate.String(), Base: t.Base,
				BaseAmount: t.BaseAmount, Amount: t.Amount,
			}
		}
		if len(l.Discounts) > 0 {
			ds := make([]api.LineDiscount, len(l.Discounts))
			for j, d := range l.Discounts {
				ds[j] = api.LineDiscount{
					Type: api.LineDiscountType(d.Type), Name: optional(d.Name), Amount: d.Amount,
				}
			}
			pl.Discounts = &ds
		}
		out.Lines[i] = pl
	}
	for i, u := range r.Unpriced {
		out.Unpriced[i] = api.Unpriced{SkuID: parseUUID(u.SKUID), Reason: api.UnpricedReason(u.Reason)}
	}
	for i, t := range r.Totals.TaxBreakdown {
		out.Totals.TaxBreakdown[i] = api.TaxTotal{Rule: t.Rule, Rate: t.Rate.String(), Amount: t.Amount}
	}
	for i, a := range r.Audit {
		out.AuditTrail[i] = api.AuditEntry{Evaluator: a.Evaluator, Detail: a.Detail}
	}
	if d := r.Display; d != nil {
		out.Display = &api.Display{
			Currency: d.Currency, Rate: d.Rate.String(), Provider: d.Provider, RateFetchedAt: d.RateFetchedAt.UTC(),
			Lines: make([]api.DisplayLine, len(d.Lines)),
			Totals: api.DisplayTotals{
				SubtotalExclTax: d.Totals.SubtotalExclTax, TaxTotal: d.Totals.TaxTotal, GrandTotal: d.Totals.GrandTotal,
			},
		}
		for i, l := range d.Lines {
			out.Display.Lines[i] = api.DisplayLine{SkuID: parseUUID(l.SKUID), UnitPrice: l.UnitPrice, Net: l.Net, Tax: l.Tax, Gross: l.Gross}
		}
	}
	return out
}

func basePrice(p domain.BasePrice) api.BasePrice {
	return api.BasePrice{SkuID: parseUUID(p.SKUID), Price: p.Price, TaxInclusive: p.TaxInclusive, ValidFrom: p.ValidFrom}
}

func salePrice(s domain.SalePrice) api.SalePrice {
	return api.SalePrice{
		SkuID: parseUUID(s.SKUID), Price: s.Price, TaxInclusive: s.TaxInclusive,
		Name: optional(s.Name), StartsAt: s.StartsAt, EndsAt: s.EndsAt, UpdatedAt: s.UpdatedAt,
	}
}

func bxgyInput(b api.BXGYCampaignInput) domain.BXGYInput {
	in := domain.BXGYInput{
		Name: b.Name, DiscountType: string(b.DiscountType), DiscountValue: b.DiscountValue,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, Enabled: b.Enabled,
	}
	if b.QualifyingProductID != nil {
		in.QualifyingProductID = b.QualifyingProductID.String()
	}
	if b.QualifyingSkuID != nil {
		in.QualifyingSKUID = b.QualifyingSkuID.String()
	}
	if b.TargetProductID != nil {
		in.TargetProductID = b.TargetProductID.String()
	}
	if b.TargetSkuID != nil {
		in.TargetSKUID = b.TargetSkuID.String()
	}
	return in
}

func bxgy(c domain.BXGYCampaign) api.BXGYCampaign {
	out := api.BXGYCampaign{
		ID: parseUUID(c.ID), Name: c.Name, DiscountType: api.BXGYCampaignDiscountType(c.DiscountType),
		DiscountValue: c.DiscountValue, StartsAt: c.StartsAt, EndsAt: c.EndsAt, Enabled: c.Enabled,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if c.QualifyingProductID != "" {
		id := parseUUID(c.QualifyingProductID)
		out.QualifyingProductID = &id
	}
	if c.QualifyingSKUID != "" {
		id := parseUUID(c.QualifyingSKUID)
		out.QualifyingSkuID = &id
	}
	if c.TargetProductID != "" {
		id := parseUUID(c.TargetProductID)
		out.TargetProductID = &id
	}
	if c.TargetSKUID != "" {
		id := parseUUID(c.TargetSKUID)
		out.TargetSkuID = &id
	}
	return out
}

func config(c domain.Config) api.PricingConfig {
	return api.PricingConfig{
		StoreCurrency: c.StoreCurrency, TaxInclusive: c.TaxInclusive, DefaultJurisdiction: c.DefaultJurisdiction, UpdatedAt: c.UpdatedAt,
	}
}

func taxClass(c domain.TaxClass) api.TaxClass {
	out := api.TaxClass{
		ClassID: c.ID, DisplayName: c.DisplayName, Rules: make([]api.TaxRule, len(c.Rules)),
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	for i, r := range c.Rules {
		out.Rules[i] = api.TaxRule{
			Jurisdiction: r.Jurisdiction, RuleName: r.Name, Rate: r.Rate.String(), Base: r.Base, SortOrder: r.SortOrder,
		}
	}
	return out
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseUUID(s string) openapi_types.UUID {
	id, _ := uuid.Parse(s)
	return id
}
