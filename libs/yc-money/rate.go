package money

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"
)

// rateDigits is the precision of Rate.String, enough for exchange rates
// stored as NUMERIC(20,10).
const rateDigits = 10

// Rate is an exact, non-negative decimal ratio: a tax rate (0.15) or an
// exchange rate (0.054). It is never a float. The zero value is 0.
type Rate struct {
	r *big.Rat
}

// ParseRate parses a decimal string such as "0.15" or "18.25".
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "/eE") {
		return Rate{}, fmt.Errorf("money: invalid rate %q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Rate{}, fmt.Errorf("money: invalid rate %q", s)
	}
	if r.Sign() < 0 {
		return Rate{}, fmt.Errorf("money: negative rate %q", s)
	}
	return Rate{r: r}, nil
}

// MustRate is ParseRate for constants; it panics on invalid input.
func MustRate(s string) Rate {
	r, err := ParseRate(s)
	if err != nil {
		panic(err)
	}
	return r
}

// RateFromRat returns a Rate equal to r, which must be non-negative.
func RateFromRat(r *big.Rat) (Rate, error) {
	if r.Sign() < 0 {
		return Rate{}, fmt.Errorf("money: negative rate %s", r.RatString())
	}
	return Rate{r: new(big.Rat).Set(r)}, nil
}

func (r Rate) rat() *big.Rat {
	if r.r == nil {
		return new(big.Rat)
	}
	return r.r
}

// Rat returns a copy of the rate as a big.Rat.
func (r Rate) Rat() *big.Rat { return new(big.Rat).Set(r.rat()) }

// IsZero reports whether the rate is 0.
func (r Rate) IsZero() bool { return r.rat().Sign() == 0 }

// Cmp compares r and o, returning -1, 0 or +1.
func (r Rate) Cmp(o Rate) int { return r.rat().Cmp(o.rat()) }

// Add returns r + o.
func (r Rate) Add(o Rate) Rate { return Rate{r: new(big.Rat).Add(r.rat(), o.rat())} }

// Mul returns r × o.
func (r Rate) Mul(o Rate) Rate { return Rate{r: new(big.Rat).Mul(r.rat(), o.rat())} }

// Inverse returns 1/r, the reverse exchange rate. It fails for 0.
func (r Rate) Inverse() (Rate, error) {
	if r.IsZero() {
		return Rate{}, fmt.Errorf("money: inverse of zero rate")
	}
	return Rate{r: new(big.Rat).Inv(r.rat())}, nil
}

// String returns the rate as a decimal with up to 10 places and no
// trailing zeros, e.g. "0.15".
func (r Rate) String() string {
	return trimDecimal(r.rat().FloatString(rateDigits))
}

// MarshalJSON writes the rate as a JSON number.
func (r Rate) MarshalJSON() ([]byte, error) {
	return []byte(r.String()), nil
}

// UnmarshalJSON reads a JSON number or a decimal string.
func (r *Rate) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(b, `"`))
	p, err := ParseRate(s)
	if err != nil {
		return err
	}
	*r = p
	return nil
}
