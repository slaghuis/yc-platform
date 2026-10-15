// Package integration holds yc-orders' clients for other services.
package integration

import (
	"context"
	"net/http"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls (httpkit.InternalClient).
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Payment refunds through yc-payment's internal API (ADR-021).
type Payment struct {
	client Caller
}

// NewPayment returns a Payment client.
func NewPayment(client Caller) *Payment {
	return &Payment{client: client}
}

// Refund is yc-payment's answer to a refund.
type Refund struct {
	// Status is pending, completed, failed, or none when the transaction
	// does not exist.
	Status      string
	AmountMinor int64
}

type refundRequest struct {
	TransactionID string `json:"transaction_id"`
	OrderID       string `json:"order_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type refundResponse struct {
	Status string `json:"status"`
	Refund *struct {
		AmountMinor int64 `json:"amount_minor"`
	} `json:"refund"`
}

// RefundAll refunds what is left of a transaction. key makes a retry
// return the first refund.
func (p *Payment) RefundAll(ctx context.Context, transactionID, orderID, reason, key string) (Refund, error) {
	var res refundResponse
	err := p.client.Call(ctx, http.MethodPost, "/internal/refund",
		httpkit.CallOptions{IdempotencyKey: key, Attempts: 3},
		refundRequest{TransactionID: transactionID, OrderID: orderID, Reason: reason}, &res)
	if err != nil {
		if prob, ok := httpkit.UpstreamProblem(err); ok && prob.Status >= 400 && prob.Status < 500 {
			return Refund{}, ycerrors.Conflict("The payment could not be refunded: " + prob.Detail).Wrap(err)
		}
		return Refund{}, err
	}
	out := Refund{Status: res.Status}
	if res.Refund != nil {
		out.AmountMinor = res.Refund.AmountMinor
	}
	return out, nil
}
