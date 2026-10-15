package integration

import (
	"context"
	"net/http"
	"net/url"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Orders is yc-orders' customer-orders API.
type Orders struct {
	client Caller
}

// NewOrders returns the client.
func NewOrders(client Caller) *Orders {
	return &Orders{client: client}
}

// OpenOrders counts userID's unfinished orders and open returns in the
// tenant of ctx.
func (o *Orders) OpenOrders(ctx context.Context, userID string) (int, error) {
	var out struct {
		OpenCount int `json:"open_count"`
	}
	err := o.client.Call(ctx, http.MethodGet, "/internal/customer-orders?user_id="+url.QueryEscape(userID),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	return out.OpenCount, err
}
