// Package domain holds yc-cart's types and rules: cart types and limits,
// the visitor merge and price change detection. It does no I/O.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// CartType is a kind of cart and its limits (docs/services/yc-cart.md,
// Cart Types).
type CartType struct {
	Name string
	// Convertible carts can start a checkout.
	Convertible bool
	// MergeOnLogin merges a visitor's cart of this type into the user's.
	MergeOnLogin bool
	// Abandonable carts are announced (cart.abandoned.v1) when a
	// customer leaves them idle.
	Abandonable bool
	// MaxItems caps the number of lines; MaxQuantity caps each line.
	MaxItems    int
	MaxQuantity int
}

var typeNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Types are the configured cart types by name.
type Types map[string]CartType

// Validate checks names and limits.
func (ts Types) Validate() error {
	if len(ts) == 0 {
		return errors.New("at least one cart type is required")
	}
	var errs []error
	for name, t := range ts {
		if !typeNameRe.MatchString(name) || t.Name != name {
			errs = append(errs, fmt.Errorf("cart type %q: name must be lower case letters, digits and _", name))
		}
		if t.MaxItems < 1 || t.MaxQuantity < 1 {
			errs = append(errs, fmt.Errorf("cart type %q: max_items and max_quantity must be positive", name))
		}
	}
	return errors.Join(errs...)
}

// Get returns the named type, or a not-found error.
func (ts Types) Get(name string) (CartType, error) {
	t, ok := ts[name]
	if !ok {
		return CartType{}, ycerrors.NotFound("Cart type", name)
	}
	return t, nil
}

// Names returns the type names in order.
func (ts Types) Names() []string {
	out := make([]string, 0, len(ts))
	for n := range ts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CheckQuantity validates a requested line quantity for t.
func (t CartType) CheckQuantity(q int) error {
	if q < 1 || q > t.MaxQuantity {
		return ycerrors.Validation().
			Field("quantity", "out_of_range", fmt.Sprintf("Must be between 1 and %d", t.MaxQuantity)).Build()
	}
	return nil
}

// Cart is a user's cart of one type.
type Cart struct {
	ID        string
	UserID    string
	Type      string
	Items     []Item
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Item returns the line for skuID.
func (c Cart) Item(skuID string) (Item, bool) {
	for _, it := range c.Items {
		if it.SKUID == skuID {
			return it, true
		}
	}
	return Item{}, false
}

// Quantity is the sum of the line quantities.
func (c Cart) Quantity() int {
	n := 0
	for _, it := range c.Items {
		n += it.Quantity
	}
	return n
}

// Item is a cart line. UnitPrice is the gross unit price in the store
// currency when the line was first added; it is kept for change detection
// and is not authoritative (checkout re-prices).
type Item struct {
	SKUID     string
	Quantity  int
	UnitPrice money.Money
	AddedAt   time.Time
	UpdatedAt time.Time
}

// MergeItems merges a visitor's lines into a user's cart of type t. It
// returns the user's lines to write and how many visitor lines were
// merged. Quantities of shared SKUs are summed, capped at MaxQuantity, and
// keep the user's price snapshot. Visitor lines are taken oldest first;
// those that would exceed MaxItems are dropped.
func MergeItems(user, visitor []Item, t CartType) (writes []Item, merged int) {
	vs := append([]Item(nil), visitor...)
	sort.SliceStable(vs, func(i, j int) bool {
		if !vs[i].AddedAt.Equal(vs[j].AddedAt) {
			return vs[i].AddedAt.Before(vs[j].AddedAt)
		}
		return vs[i].SKUID < vs[j].SKUID
	})
	have := make(map[string]Item, len(user))
	for _, it := range user {
		have[it.SKUID] = it
	}
	lines := len(user)
	for _, v := range vs {
		if u, ok := have[v.SKUID]; ok {
			u.Quantity = min(u.Quantity+v.Quantity, t.MaxQuantity)
			writes = append(writes, u)
			merged++
			continue
		}
		if lines >= t.MaxItems {
			continue
		}
		v.Quantity = min(v.Quantity, t.MaxQuantity)
		writes = append(writes, v)
		lines++
		merged++
	}
	return writes, merged
}

// ConvertItems removes checked-out quantities from a cart's lines. Lines
// whose quantity falls to zero are deleted; the rest are written with the
// remainder. removed holds the quantity actually taken per SKU, which is
// less than checked out when the customer lowered a line meanwhile.
func ConvertItems(lines []Item, checkedOut map[string]int) (writes []Item, deletes []string, removed map[string]int) {
	removed = map[string]int{}
	for _, it := range lines {
		q := checkedOut[it.SKUID]
		if q <= 0 {
			continue
		}
		take := min(q, it.Quantity)
		removed[it.SKUID] = take
		if it.Quantity == take {
			deletes = append(deletes, it.SKUID)
			continue
		}
		it.Quantity -= take
		writes = append(writes, it)
	}
	return writes, deletes, removed
}
