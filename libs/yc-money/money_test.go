package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestAddSub(t *testing.T) {
	a, b := New(1000, "ZAR"), New(250, "ZAR")
	if got, _ := a.Add(b); got != New(1250, "ZAR") {
		t.Errorf("Add = %v", got)
	}
	if got, _ := a.Sub(b); got != New(750, "ZAR") {
		t.Errorf("Sub = %v", got)
	}
	if _, err := a.Add(New(1, "USD")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add mismatched currencies: err = %v", err)
	}
	if _, err := New(math.MaxInt64, "ZAR").Add(New(1, "ZAR")); !errors.Is(err, ErrOverflow) {
		t.Errorf("Add overflow: err = %v", err)
	}
	if _, err := New(math.MinInt64, "ZAR").Sub(New(1, "ZAR")); !errors.Is(err, ErrOverflow) {
		t.Errorf("Sub overflow: err = %v", err)
	}
}

func TestTimes(t *testing.T) {
	if got, _ := New(1999, "ZAR").Times(3); got.AmountMinor != 5997 {
		t.Errorf("Times = %v", got)
	}
	if _, err := New(math.MaxInt64/2+1, "ZAR").Times(2); !errors.Is(err, ErrOverflow) {
		t.Errorf("Times overflow: err = %v", err)
	}
}

func TestMulRoundsHalfAwayFromZero(t *testing.T) {
	tests := []struct {
		amount int64
		rate   string
		want   int64
	}{
		{10000, "0.15", 1500},
		{1, "0.5", 1},   // 0.5 → 1
		{-1, "0.5", -1}, // -0.5 → -1
		{3, "0.15", 0},  // 0.45 → 0
		{10, "0.15", 2}, // 1.5 → 2
		{333, "0.333333", 111},
	}
	for _, tt := range tests {
		got, err := New(tt.amount, "ZAR").Mul(MustRate(tt.rate))
		if err != nil || got.AmountMinor != tt.want {
			t.Errorf("%d × %s = %v, %v; want %d", tt.amount, tt.rate, got, err, tt.want)
		}
	}
}

func TestConvert(t *testing.T) {
	tests := []struct {
		from Money
		to   string
		rate string
		want Money
	}{
		{New(19000, "ZAR"), "USD", "0.054", New(1026, "USD")},
		{New(10000, "ZAR"), "JPY", "8", New(800, "JPY")},
		{New(800, "JPY"), "ZAR", "0.125", New(10000, "ZAR")},
		{New(10000, "ZAR"), "KWD", "0.0166", New(1660, "KWD")},
		{New(10000, "ZAR"), "ZAR", "1", New(10000, "ZAR")},
	}
	for _, tt := range tests {
		got, err := tt.from.Convert(tt.to, MustRate(tt.rate))
		if err != nil || got != tt.want {
			t.Errorf("%v → %s at %s = %v, %v; want %v", tt.from, tt.to, tt.rate, got, err, tt.want)
		}
	}
}

func TestAllocateSumsToTotal(t *testing.T) {
	tests := []struct {
		total   int64
		weights []int64
		want    []int64
	}{
		{100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{-100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{5, []int64{3, 0, 7}, []int64{2, 0, 3}},
		{2608, []int64{10000, 10000}, []int64{1304, 1304}},
		{1, []int64{0, 5}, []int64{0, 1}},
	}
	for _, tt := range tests {
		parts, err := Allocate(New(tt.total, "ZAR"), tt.weights)
		if err != nil {
			t.Fatalf("Allocate(%d, %v): %v", tt.total, tt.weights, err)
		}
		var sum int64
		for i, p := range parts {
			sum += p.AmountMinor
			if p.AmountMinor != tt.want[i] {
				t.Errorf("Allocate(%d, %v)[%d] = %d, want %d", tt.total, tt.weights, i, p.AmountMinor, tt.want[i])
			}
		}
		if sum != tt.total {
			t.Errorf("Allocate(%d, %v) sums to %d", tt.total, tt.weights, sum)
		}
	}
	if _, err := Allocate(New(1, "ZAR"), []int64{0, 0}); err == nil {
		t.Error("Allocate with zero weights succeeded")
	}
	if _, err := Allocate(New(1, "ZAR"), []int64{-1, 2}); err == nil {
		t.Error("Allocate with a negative weight succeeded")
	}
}

func TestString(t *testing.T) {
	tests := map[Money]string{
		New(12995, "ZAR"): "ZAR 129.95",
		New(-5, "ZAR"):    "ZAR -0.05",
		New(800, "JPY"):   "JPY 800",
		New(1660, "KWD"):  "KWD 1.660",
	}
	for m, want := range tests {
		if got := m.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestExponentAndCurrency(t *testing.T) {
	if Exponent("ZAR") != 2 || Exponent("JPY") != 0 || Exponent("KWD") != 3 {
		t.Error("unexpected exponents")
	}
	if !ValidCurrency("ZAR") || ValidCurrency("zar") || ValidCurrency("ZA") {
		t.Error("ValidCurrency is wrong")
	}
}

func TestRate(t *testing.T) {
	for _, bad := range []string{"", "-0.1", "1/3", "1e3", "abc"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) succeeded", bad)
		}
	}
	r := MustRate("0.150000")
	if r.String() != "0.15" {
		t.Errorf("String() = %q", r.String())
	}
	inv, err := MustRate("0.125").Inverse()
	if err != nil || inv.String() != "8" {
		t.Errorf("Inverse = %v, %v", inv, err)
	}
	if _, err := (Rate{}).Inverse(); err == nil {
		t.Error("Inverse of zero succeeded")
	}
	if MustRate("0.15").Add(MustRate("1")).String() != "1.15" {
		t.Error("Add is wrong")
	}

	b, _ := json.Marshal(struct {
		Rate Rate `json:"rate"`
	}{MustRate("0.054")})
	if string(b) != `{"rate":0.054}` {
		t.Errorf("Marshal = %s", b)
	}
	var got struct {
		A Rate `json:"a"`
		B Rate `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":0.15,"b":"18.25"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.A.String() != "0.15" || got.B.String() != "18.25" {
		t.Errorf("Unmarshal = %v %v", got.A, got.B)
	}
}
