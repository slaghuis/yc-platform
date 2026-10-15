// Package integration calls other services' internal APIs through
// yc-gateway (ADR-021): yc-orders for verified purchases and delivered
// orders' products.
package integration

import (
	"context"
	"net/http"
	"net/url"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls.
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Orders is yc-orders' internal API.
type Orders struct {
	client Caller
}

// NewOrders returns the client.
func NewOrders(client Caller) *Orders {
	return &Orders{client: client}
}

// Purchase is a customer's latest order of a product, if any.
type Purchase struct {
	Purchased bool   `json:"purchased"`
	OrderID   string `json:"order_id"`
}

// LatestPurchase reports whether the customer bought the product.
func (o *Orders) LatestPurchase(ctx context.Context, userID, productID string) (Purchase, error) {
	var out Purchase
	q := url.Values{"user_id": {userID}, "product_id": {productID}}
	err := o.client.Call(ctx, http.MethodGet, "/internal/purchases?"+q.Encode(),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	return out, err
}

// OrderItem is an order line.
type OrderItem struct {
	ProductID    string `json:"product_id"`
	NameSnapshot string `json:"name_snapshot"`
}

// Order is what yc-review needs of an order.
type Order struct {
	OrderID     string      `json:"order_id"`
	OrderNumber string      `json:"order_number"`
	UserID      string      `json:"user_id"`
	UserType    string      `json:"user_type"`
	Items       []OrderItem `json:"items"`
}

// Order returns an order.
func (o *Orders) Order(ctx context.Context, orderID string) (Order, error) {
	var out Order
	err := o.client.Call(ctx, http.MethodGet, "/internal/orders/"+url.PathEscape(orderID),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	return out, err
}
