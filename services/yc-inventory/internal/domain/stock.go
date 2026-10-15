// Package domain holds yc-inventory's stock model and rules.
package domain

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Level is a tracked SKU's stock.
type Level struct {
	SKUID        string
	OnHand       int64
	Reserved     int64
	LowThreshold int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Available is on_hand minus reserved; negative only after an oversell.
func (l Level) Available() int64 { return l.OnHand - l.Reserved }

func (l Level) belowThreshold() bool {
	return l.LowThreshold > 0 && l.Available() < l.LowThreshold
}

// Signal is a change in a level that other services act on.
type Signal int

// Signals, in the order they are published.
const (
	SignalDepleted Signal = iota + 1 // available dropped to zero or below
	SignalRestored                   // available rose above zero
	SignalLow                        // available crossed below the low threshold
)

// Signals reports what changed between before and after.
func Signals(before, after Level) []Signal {
	var out []Signal
	switch {
	case before.Available() > 0 && after.Available() <= 0:
		out = append(out, SignalDepleted)
	case before.Available() <= 0 && after.Available() > 0:
		out = append(out, SignalRestored)
	}
	if !before.belowThreshold() && after.belowThreshold() {
		out = append(out, SignalLow)
	}
	return out
}

// Receive adds quantity to on_hand.
func (l Level) Receive(quantity int64) (Level, error) {
	if quantity <= 0 {
		return l, ycerrors.Validation().Field("quantity", "out_of_range", "Quantity must be positive").Build()
	}
	l.OnHand += quantity
	return l, nil
}

// Adjust changes on_hand by delta. It refuses to take on_hand below zero
// or below what is reserved; release reservations first.
func (l Level) Adjust(delta int64) (Level, error) {
	if delta == 0 {
		return l, ycerrors.Validation().Field("delta", "out_of_range", "Delta must not be zero").Build()
	}
	l.OnHand += delta
	if l.OnHand < 0 {
		return l, ycerrors.Validation().Field("delta", "out_of_range", "On-hand stock cannot go below zero").Build()
	}
	if delta < 0 && l.OnHand < l.Reserved {
		return l, ycerrors.Conflict(fmt.Sprintf("%d units are reserved; release reservations before adjusting below that", l.Reserved))
	}
	return l, nil
}

// SetThreshold sets the low-stock threshold; 0 turns warnings off.
func (l Level) SetThreshold(t int64) (Level, error) {
	if t < 0 {
		return l, ycerrors.Validation().Field("low_threshold", "out_of_range", "Threshold must not be negative").Build()
	}
	l.LowThreshold = t
	return l, nil
}

// Reserve holds quantity, which the caller has checked is available.
func (l Level) Reserve(quantity int64) Level {
	l.Reserved += quantity
	return l
}

// Release returns a reserved quantity to available.
func (l Level) Release(quantity int64) Level {
	l.Reserved = max(l.Reserved-quantity, 0)
	return l
}

// Commit takes quantity out of on_hand for a placed order. An active
// reservation also gives up its hold. An expired one no longer holds
// stock, so whatever exceeds what was available is oversold: the order
// stands and the shop admin sees negative available stock.
func (l Level) Commit(quantity int64, active bool) (Level, int64) {
	var oversold int64
	if active {
		l.Reserved = max(l.Reserved-quantity, 0)
	} else {
		oversold = max(quantity-max(l.Available(), 0), 0)
	}
	l.OnHand = max(l.OnHand-quantity, 0)
	return l, oversold
}

// Config is a shop's inventory settings.
type Config struct {
	Enabled    bool
	DefaultTTL int // minutes
	MinTTL     int
	MaxTTL     int
	Stored     bool
	UpdatedAt  time.Time
}

// Validate checks min ≤ default ≤ max, all positive and at most a day.
func (c Config) Validate() error {
	v := ycerrors.Validation()
	if c.MinTTL < 1 || c.MaxTTL > 24*60 {
		v.Field("min_reservation_ttl_minutes", "out_of_range", "Reservation TTLs are between 1 minute and 24 hours")
	}
	if c.MinTTL > c.DefaultTTL || c.DefaultTTL > c.MaxTTL {
		v.Field("default_reservation_ttl_minutes", "out_of_range", "The default TTL must lie between the minimum and maximum")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// TTL is the reservation lifetime for a requested number of minutes,
// clamped to the shop's window; nil means the default.
func (c Config) TTL(requested *int) time.Duration {
	m := c.DefaultTTL
	if requested != nil {
		m = min(max(*requested, c.MinTTL), c.MaxTTL)
	}
	return time.Duration(m) * time.Minute
}

// ReservationStatus is a reservation's state.
type ReservationStatus string

// Reservation states.
const (
	ReservationActive    ReservationStatus = "active"
	ReservationCommitted ReservationStatus = "committed"
	ReservationReleased  ReservationStatus = "released"
	ReservationExpired   ReservationStatus = "expired"
)

// Line is a SKU and quantity.
type Line struct {
	SKUID    string
	Quantity int64
}

// Reservation holds tracked stock for a checkout session.
type Reservation struct {
	ID             string
	SessionID      string
	IdempotencyKey string
	Status         ReservationStatus
	ExpiresAt      time.Time
	CreatedAt      time.Time
	SettledAt      *time.Time
	Items          []Line
}

// Shortage is a line that cannot be reserved.
type Shortage struct {
	SKUID     string `json:"sku_id"`
	Requested int64  `json:"requested"`
	Available int64  `json:"available"`
}

// MaxLines bounds a reservation request.
const MaxLines = 100

// NormaliseLines validates lines and merges repeated SKUs, sorted by SKU
// so rows are locked in a fixed order.
func NormaliseLines(lines []Line) ([]Line, error) {
	v := ycerrors.Validation()
	if len(lines) == 0 || len(lines) > MaxLines {
		v.Field("items", "out_of_range", fmt.Sprintf("Between 1 and %d items", MaxLines))
	}
	merged := map[string]int64{}
	for i, l := range lines {
		if _, err := uuid.Parse(l.SKUID); err != nil {
			v.Field(fmt.Sprintf("items[%d].sku_id", i), "invalid", "SKU ID is not a UUID")
		}
		if l.Quantity <= 0 {
			v.Field(fmt.Sprintf("items[%d].quantity", i), "out_of_range", "Quantity must be positive")
		}
		merged[l.SKUID] += l.Quantity
	}
	if v.HasErrors() {
		return nil, v.Build()
	}
	out := make([]Line, 0, len(merged))
	for sku, q := range merged {
		out = append(out, Line{SKUID: sku, Quantity: q})
	}
	slices.SortFunc(out, func(a, b Line) int { return cmp.Compare(a.SKUID, b.SKUID) })
	return out, nil
}

// MovementType is the kind of a stock movement.
type MovementType string

// Movement types.
const (
	MovementReceive MovementType = "receive"
	MovementAdjust  MovementType = "adjust"
	MovementReserve MovementType = "reserve"
	MovementRelease MovementType = "release"
	MovementCommit  MovementType = "commit"
	MovementExpire  MovementType = "expire"
)

// Movement is one change to a level.
type Movement struct {
	ID            string
	SKUID         string
	Type          MovementType
	OnHandDelta   int64
	ReservedDelta int64
	Reason        string
	ActorID       string
	Reference     string
	CreatedAt     time.Time
}
