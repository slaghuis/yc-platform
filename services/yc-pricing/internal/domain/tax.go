package domain

import (
	"math/big"
	"strings"

	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// TaxAmount is one rule's tax on a line.
type TaxAmount struct {
	Jurisdiction string
	Rule         string
	Rate         money.Rate
	Base         string
	BaseAmount   money.Money
	Amount       money.Money
}

// ApplyTaxes levies rules (in evaluation order) on a line amount. With
// inclusive the amount already contains the taxes and they are extracted;
// otherwise they are added. Either way net + Σ taxes = gross exactly.
//
// Extraction divides the gross by the combined multiplier, so a 15% VAT on
// 200.00 gives net 173.91 and VAT 26.09. Each rule is then computed on the
// net as if exclusive, and the last rule absorbs any rounding cent.
func ApplyTaxes(amount money.Money, inclusive bool, rules []TaxRule) (net, gross money.Money, taxes []TaxAmount, err error) {
	if !inclusive {
		if taxes, err = levy(amount, rules); err != nil {
			return money.Money{}, money.Money{}, nil, err
		}
		gross, err = addTaxes(amount, taxes)
		return amount, gross, taxes, err
	}

	multiplier := big.NewRat(1, 1)
	coeff := make(map[string]*big.Rat, len(rules))
	for _, r := range rules {
		c := r.Rate.Rat()
		if ref, ok := strings.CutPrefix(r.Base, BaseIncludingPrefix); ok {
			c.Mul(c, new(big.Rat).Add(big.NewRat(1, 1), coeff[ref]))
		}
		coeff[r.Name] = c
		multiplier.Add(multiplier, c)
	}
	toNet, err := money.RateFromRat(new(big.Rat).Inv(multiplier))
	if err != nil {
		return money.Money{}, money.Money{}, nil, err
	}
	if net, err = amount.Mul(toNet); err != nil {
		return money.Money{}, money.Money{}, nil, err
	}
	if taxes, err = levy(net, rules); err != nil {
		return money.Money{}, money.Money{}, nil, err
	}
	if len(taxes) > 0 {
		sum, err := addTaxes(net, taxes)
		if err != nil {
			return money.Money{}, money.Money{}, nil, err
		}
		last := &taxes[len(taxes)-1]
		last.Amount.AmountMinor += amount.AmountMinor - sum.AmountMinor
	}
	return net, amount, taxes, nil
}

func levy(net money.Money, rules []TaxRule) ([]TaxAmount, error) {
	taxes := make([]TaxAmount, 0, len(rules))
	byName := make(map[string]money.Money, len(rules))
	for _, r := range rules {
		base := net
		if ref, ok := strings.CutPrefix(r.Base, BaseIncludingPrefix); ok {
			var err error
			if base, err = base.Add(byName[ref]); err != nil {
				return nil, err
			}
		}
		amt, err := base.Mul(r.Rate)
		if err != nil {
			return nil, err
		}
		byName[r.Name] = amt
		taxes = append(taxes, TaxAmount{
			Jurisdiction: r.Jurisdiction, Rule: r.Name, Rate: r.Rate, Base: r.Base, BaseAmount: base, Amount: amt,
		})
	}
	return taxes, nil
}

func addTaxes(net money.Money, taxes []TaxAmount) (money.Money, error) {
	total := net
	for _, t := range taxes {
		var err error
		if total, err = total.Add(t.Amount); err != nil {
			return money.Money{}, err
		}
	}
	return total, nil
}
