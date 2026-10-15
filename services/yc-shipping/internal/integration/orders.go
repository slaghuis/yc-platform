// Package integration holds yc-shipping's clients for other services,
// called through yc-gateway's internal listener (ADR-021).
package integration

import (
	"context"
	"net/http"
	"net/url"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller is httpkit.InternalClient.
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Orders is yc-orders' customer-orders API.
type Orders struct {
	client Caller
}

// NewOrders returns the client.
func NewOrders(client Caller) *Orders {
	return &Orders{client: client}
}

// CustomerOrderIDs returns the IDs of the orders placed by any of userIDs
// in the tenant of ctx.
func (o *Orders) CustomerOrderIDs(ctx context.Context, userIDs ...string) ([]string, error) {
	var out struct {
		Orders []struct {
			OrderID string `json:"order_id"`
		} `json:"orders"`
	}
	err := o.client.Call(ctx, http.MethodGet, "/internal/customer-orders?"+url.Values{"user_id": userIDs}.Encode(),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Orders))
	for i, o := range out.Orders {
		ids[i] = o.OrderID
	}
	return ids, nil
}
