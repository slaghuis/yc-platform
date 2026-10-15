// Package integration holds yc-cart's clients for other services.
package integration

import (
	"context"
	"net/http"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
)

// Doer makes internal calls (httpkit.InternalClient).
type Doer interface {
	Do(ctx context.Context, method, path string, in, out any) error
}

// Pricing prices SKUs with yc-pricing's internal evaluation (ADR-021).
type Pricing struct {
	client Doer
}

// NewPricing returns a Pricing client.
func NewPricing(client Doer) *Pricing {
	return &Pricing{client: client}
}

type evaluateLine struct {
	SKUID    string `json:"sku_id"`
	Quantity int    `json:"quantity"`
}

type evaluateRequest struct {
	Lines           []evaluateLine `json:"lines"`
	DisplayCurrency string         `json:"display_currency,omitempty"`
}

type evaluateResponse struct {
	StoreCurrency string `json:"store_currency"`
	Lines         []struct {
		SKUID string      `json:"sku_id"`
		Gross money.Money `json:"gross"`
	} `json:"lines"`
	Unpriced []struct {
		SKUID  string `json:"sku_id"`
		Reason string `json:"reason"`
	} `json:"unpriced"`
	Display *struct {
		Currency      string    `json:"currency"`
		Rate          string    `json:"rate"`
		RateFetchedAt time.Time `json:"rate_fetched_at"`
		Lines         []struct {
			SKUID string      `json:"sku_id"`
			Gross money.Money `json:"gross"`
		} `json:"lines"`
	} `json:"display"`
}

// maxLines is yc-pricing's limit per evaluation.
const maxLines = 100

// UnitPrices returns the gross unit prices of skuIDs, converted to
// displayCurrency when given. Without an exchange rate the prices come back
// in the store currency only.
func (p *Pricing) UnitPrices(ctx context.Context, skuIDs []string, displayCurrency string) (domain.Prices, error) {
	out := domain.Prices{Units: map[string]money.Money{}, Unpriced: map[string]string{}}
	for start := 0; start < len(skuIDs); start += maxLines {
		batch := skuIDs[start:min(start+maxLines, len(skuIDs))]
		res, err := p.evaluate(ctx, batch, displayCurrency)
		if err != nil && displayCurrency != "" && httpkit.UpstreamStatus(err) == http.StatusBadRequest {
			displayCurrency = ""
			out.Display = nil
			res, err = p.evaluate(ctx, batch, "")
		}
		if err != nil {
			return domain.Prices{}, err
		}
		merge(&out, res, displayCurrency)
	}
	return out, nil
}

func (p *Pricing) evaluate(ctx context.Context, skuIDs []string, displayCurrency string) (evaluateResponse, error) {
	req := evaluateRequest{Lines: make([]evaluateLine, len(skuIDs)), DisplayCurrency: displayCurrency}
	for i, id := range skuIDs {
		req.Lines[i] = evaluateLine{SKUID: id, Quantity: 1}
	}
	var res evaluateResponse
	err := p.client.Do(ctx, http.MethodPost, "/internal/prices/evaluate", req, &res)
	if httpkit.UpstreamStatus(err) == http.StatusConflict {
		return evaluateResponse{}, ycerrors.Conflict("Prices are not available for this shop").Wrap(err)
	}
	return res, err
}

func merge(out *domain.Prices, res evaluateResponse, displayCurrency string) {
	out.StoreCurrency = res.StoreCurrency
	for _, l := range res.Lines {
		out.Units[l.SKUID] = l.Gross
	}
	for _, u := range res.Unpriced {
		out.Unpriced[u.SKUID] = u.Reason
	}
	if res.Display == nil || displayCurrency == "" {
		return
	}
	if out.Display == nil {
		out.Display = &domain.DisplayPrices{
			Currency: res.Display.Currency, Rate: res.Display.Rate, RateFetchedAt: res.Display.RateFetchedAt,
			Units: map[string]money.Money{},
		}
	}
	for _, l := range res.Display.Lines {
		out.Display.Units[l.SKUID] = l.Gross
	}
}
