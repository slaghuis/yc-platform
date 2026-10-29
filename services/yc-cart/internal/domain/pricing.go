package domain

import (
	"time"

	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// LineQty is one cart line quantity for a pricing evaluation.
type LineQty struct {
	SKUID    string
	Quantity int
}

// Prices are current gross unit prices from yc-pricing.
type Prices struct {
	StoreCurrency string
	// Units are gross unit prices in the store currency by SKU.
	Units map[string]money.Money
	// Unpriced gives the reason for SKUs that cannot be sold.
	Unpriced map[string]string
	// Display is the conversion into the requested display currency; nil
	// when none was requested, it is the store currency, or no rate exists.
	Display *DisplayPrices
}

// DisplayPrices are unit prices converted for display (ADR-014: cosmetic).
type DisplayPrices struct {
	Currency      string
	Rate          string
	RateFetchedAt time.Time
	Units         map[string]money.Money
}

// View is a cart priced at current prices.
type View struct {
	Cart        Cart
	Type        CartType
	Lines       []LineView
	Currency    string
	Subtotal    money.Money
	PriceChange bool
	Display     *DisplayView
}

// LineView is a line with its current price.
type LineView struct {
	Item
	// Available is false when the SKU can no longer be priced; Reason says
	// why and the line is left out of the totals.
	Available bool
	Reason    string
	Current   money.Money
	Total     money.Money
	// PriceChanged reports that Current differs from the snapshot.
	PriceChanged bool
	Display      *LineDisplay
}

// LineDisplay is a line in the display currency.
type LineDisplay struct {
	UnitPrice money.Money
	Total     money.Money
}

// DisplayView is the cart total in the display currency.
type DisplayView struct {
	Currency      string
	Rate          string
	RateFetchedAt time.Time
	Subtotal      money.Money
}

// Price prices c at p. Display amounts convert each unit price and
// multiply, so they may differ from a converted store total by rounding.
func Price(c Cart, t CartType, p Prices) (View, error) {
	v := View{Cart: c, Type: t, Currency: p.StoreCurrency, Subtotal: money.Zero(p.StoreCurrency), Lines: make([]LineView, len(c.Items))}
	if p.Display != nil {
		v.Display = &DisplayView{
			Currency: p.Display.Currency, Rate: p.Display.Rate, RateFetchedAt: p.Display.RateFetchedAt,
			Subtotal: money.Zero(p.Display.Currency),
		}
	}
	for i, it := range c.Items {
		l := LineView{Item: it}
		cur, ok := p.Units[it.SKUID]
		if !ok {
			l.Reason = p.Unpriced[it.SKUID]
			if l.Reason == "" {
				l.Reason = "unpriced"
			}
			v.Lines[i] = l
			continue
		}
		total, err := cur.Times(int64(it.Quantity))
		if err != nil {
			return View{}, err
		}
		l.Available, l.Current, l.Total = true, cur, total
		l.PriceChanged = cur != it.UnitPrice
		v.PriceChange = v.PriceChange || l.PriceChanged
		if v.Subtotal, err = v.Subtotal.Add(total); err != nil {
			return View{}, err
		}
		if d, ok := displayUnit(p, it.SKUID); ok && v.Display != nil {
			dt, err := d.Times(int64(it.Quantity))
			if err != nil {
				return View{}, err
			}
			l.Display = &LineDisplay{UnitPrice: d, Total: dt}
			if v.Display.Subtotal, err = v.Display.Subtotal.Add(dt); err != nil {
				return View{}, err
			}
		}
		v.Lines[i] = l
	}
	return v, nil
}

func displayUnit(p Prices, skuID string) (money.Money, bool) {
	if p.Display == nil {
		return money.Money{}, false
	}
	m, ok := p.Display.Units[skuID]
	return m, ok
}
