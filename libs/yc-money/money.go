package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
)

var (
	// ErrCurrencyMismatch is returned when combining amounts in different
	// currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in int64 minor
	// units.
	ErrOverflow = errors.New("money: overflow")
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidCurrency reports whether code is shaped like an ISO 4217 code.
func ValidCurrency(code string) bool {
	return currencyPattern.MatchString(code)
}

// exponents lists ISO 4217 currencies whose minor unit is not 1/100.
var exponents = map[string]int{
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0,
	"KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "VND": 0, "VUV": 0, "XAF": 0,
	"XOF": 0, "XPF": 0,
}

// Exponent returns the number of decimal places in currency's minor unit:
// 2 for ZAR (cents), 0 for JPY, 3 for KWD.
func Exponent(currency string) int {
	if e, ok := exponents[currency]; ok {
		return e
	}
	return 2
}

// Money is an amount in integer minor units of a currency. The zero value
// has no currency and is only useful as a placeholder.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// New returns amountMinor of currency.
func New(amountMinor int64, currency string) Money {
	return Money{AmountMinor: amountMinor, Currency: currency}
}

// Zero returns nothing of currency.
func Zero(currency string) Money {
	return Money{Currency: currency}
}

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.AmountMinor == 0 }

// IsNegative reports whether the amount is below zero.
func (m Money) IsNegative() bool { return m.AmountMinor < 0 }

// Neg returns -m. It panics on math.MinInt64, which no real amount reaches.
func (m Money) Neg() Money {
	if m.AmountMinor == math.MinInt64 {
		panic(ErrOverflow)
	}
	return Money{AmountMinor: -m.AmountMinor, Currency: m.Currency}
}

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	a, b := m.AmountMinor, o.AmountMinor
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, ErrOverflow
	}
	return Money{AmountMinor: a + b, Currency: m.Currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if o.AmountMinor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return m.Add(Money{AmountMinor: -o.AmountMinor, Currency: o.Currency})
}

// Times returns m multiplied by a whole quantity.
func (m Money) Times(qty int64) (Money, error) {
	p := new(big.Int).Mul(big.NewInt(m.AmountMinor), big.NewInt(qty))
	if !p.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{AmountMinor: p.Int64(), Currency: m.Currency}, nil
}

// Mul returns m multiplied by r, rounded half away from zero to a whole
// minor unit. Use it for percentages: a 15% tax on m is m.Mul(rate 0.15).
func (m Money) Mul(r Rate) (Money, error) {
	v, err := roundHalfUp(new(big.Rat).Mul(new(big.Rat).SetInt64(m.AmountMinor), r.rat()))
	if err != nil {
		return Money{}, err
	}
	return Money{AmountMinor: v, Currency: m.Currency}, nil
}

// Convert returns m in currency to, at rate units of to per one unit of
// m's currency (major units, as exchange rates are quoted), rounded half
// away from zero. Minor-unit exponents are accounted for: 100.00 ZAR at
// 8.0 JPY/ZAR is 800 JPY.
func (m Money) Convert(to string, r Rate) (Money, error) {
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(m.AmountMinor), r.rat())
	shift := Exponent(to) - Exponent(m.Currency)
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(shift))), nil))
	if shift > 0 {
		v.Mul(v, scale)
	} else if shift < 0 {
		v.Quo(v, scale)
	}
	out, err := roundHalfUp(v)
	if err != nil {
		return Money{}, err
	}
	return Money{AmountMinor: out, Currency: to}, nil
}

// String formats m in major units, e.g. "ZAR 129.95".
func (m Money) String() string {
	e := Exponent(m.Currency)
	if e == 0 {
		return fmt.Sprintf("%s %d", m.Currency, m.AmountMinor)
	}
	sign := ""
	v := new(big.Int).SetInt64(m.AmountMinor)
	if v.Sign() < 0 {
		sign = "-"
		v.Neg(v)
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(e)), nil)
	q, r := new(big.Int).QuoRem(v, pow, new(big.Int))
	return fmt.Sprintf("%s %s%s.%0*s", m.Currency, sign, q, e, r)
}

// Sum adds amounts, all of which must be in currency.
func Sum(currency string, amounts ...Money) (Money, error) {
	total := Zero(currency)
	for _, a := range amounts {
		var err error
		if total, err = total.Add(a); err != nil {
			return Money{}, err
		}
	}
	return total, nil
}

// Allocate splits total across weights in proportion, by the largest
// remainder method, so the parts always sum to total. Ties go to the
// earlier weight. Weights must be non-negative with a positive sum.
func Allocate(total Money, weights []int64) ([]Money, error) {
	sum := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("money: negative weight %d", w)
		}
		sum.Add(sum, big.NewInt(w))
	}
	if sum.Sign() == 0 {
		return nil, errors.New("money: weights sum to zero")
	}

	sign := int64(1)
	amount := big.NewInt(total.AmountMinor)
	if amount.Sign() < 0 {
		sign = -1
		amount.Neg(amount)
	}

	parts := make([]Money, len(weights))
	rems := make([]*big.Int, len(weights))
	allocated := new(big.Int)
	for i, w := range weights {
		q, r := new(big.Int).QuoRem(new(big.Int).Mul(amount, big.NewInt(w)), sum, new(big.Int))
		parts[i] = Money{AmountMinor: q.Int64(), Currency: total.Currency}
		rems[i] = r
		allocated.Add(allocated, q)
	}
	left := new(big.Int).Sub(amount, allocated).Int64()
	for ; left > 0; left-- {
		best := -1
		for i, r := range rems {
			if weights[i] > 0 && (best < 0 || r.Cmp(rems[best]) > 0) {
				best = i
			}
		}
		parts[best].AmountMinor++
		rems[best] = new(big.Int)
	}
	for i := range parts {
		parts[i].AmountMinor *= sign
	}
	return parts, nil
}

// roundHalfUp rounds v to the nearest integer, halves away from zero.
func roundHalfUp(v *big.Rat) (int64, error) {
	num, den := new(big.Int).Set(v.Num()), v.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Lsh(r, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// trimDecimal removes trailing zeros (and a trailing point) from a
// fixed-point decimal string.
func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
