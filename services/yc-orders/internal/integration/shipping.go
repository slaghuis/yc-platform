package integration

import (
	"context"
	"net/http"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Shipping books shipments through yc-shipping's internal API (ADR-022).
type Shipping struct {
	client Caller
}

// NewShipping returns a Shipping client.
func NewShipping(client Caller) *Shipping {
	return &Shipping{client: client}
}

// ShipmentRequest asks for a packaged order's shipment.
type ShipmentRequest struct {
	OrderID         string         `json:"order_id"`
	OrderNumber     string         `json:"order_number"`
	Method          string         `json:"method"`
	RateID          string         `json:"rate_id,omitempty"`
	DeliveryAddress map[string]any `json:"delivery_address"`
}

// Shipment is the booked shipment.
type Shipment struct {
	ShipmentID     string `json:"shipment_id"`
	TrackingNumber string `json:"tracking_number"`
	Status         string `json:"status"`
}

// CreateShipment books the order's shipment; yc-shipping returns the
// existing one when asked again. key makes a retry return the first
// answer.
func (s *Shipping) CreateShipment(ctx context.Context, req ShipmentRequest, key string) (Shipment, error) {
	var out Shipment
	err := s.client.Call(ctx, http.MethodPost, "/internal/shipments",
		httpkit.CallOptions{IdempotencyKey: key, Attempts: 3}, req, &out)
	if err != nil {
		if prob, ok := httpkit.UpstreamProblem(err); ok && prob.Status >= 400 && prob.Status < 500 {
			return Shipment{}, ycerrors.Conflict("The shipment could not be booked: " + prob.Detail).Wrap(err)
		}
		return Shipment{}, err
	}
	return out, nil
}
