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
	AmountMinor   int64  `json:"amount_minor,omitempty"`
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
	return p.refund(ctx, refundRequest{TransactionID: transactionID, OrderID: orderID, Reason: reason}, key)
}

// Refund refunds amountMinor of a transaction. A refund the gateway
// failed, or a transaction yc-payment does not know, is a conflict.
func (p *Payment) Refund(ctx context.Context, transactionID, orderID string, amountMinor int64, reason, key string) (Refund, error) {
	r, err := p.refund(ctx, refundRequest{
		TransactionID: transactionID, AmountMinor: amountMinor, OrderID: orderID, Reason: reason,
	}, key)
	if err != nil {
		return r, err
	}
	switch r.Status {
	case "failed":
		return r, ycerrors.Conflict("The payment gateway refused the refund")
	case "none":
		return r, ycerrors.Conflict("The order's payment was not found")
	}
	if r.AmountMinor == 0 {
		r.AmountMinor = amountMinor
	}
	return r, nil
}

func (p *Payment) refund(ctx context.Context, req refundRequest, key string) (Refund, error) {
	var res refundResponse
	err := p.client.Call(ctx, http.MethodPost, "/internal/refund",
		httpkit.CallOptions{IdempotencyKey: key, Attempts: 3}, req, &res)
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
