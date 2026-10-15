// Package carrier is the interface yc-shipping's carrier drivers implement,
// and the manual driver: the shop ships with a courier of its choice and
// reports the parcel's progress itself.
package carrier

import (
	"context"

	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
)

// Label is what a driver issues for a new shipment.
type Label struct {
	TrackingNumber string
	LabelURL       string
}

// Driver is a carrier integration.
type Driver interface {
	Name() string
	// CreateLabel books the shipment with the carrier.
	CreateLabel(ctx context.Context, s domain.Shipment) (Label, error)
	// Cancel withdraws a shipment that has not left the shop.
	Cancel(ctx context.Context, s domain.Shipment) error
	// ManualUpdates reports whether shop admins report the shipment's
	// progress, rather than the carrier.
	ManualUpdates() bool
}

// Manual is the driver without a carrier integration. Its tracking numbers
// are yc-shipping's own.
type Manual struct{}

// ManualName is the manual driver's name.
const ManualName = "manual"

// Name implements Driver.
func (Manual) Name() string { return ManualName }

// CreateLabel implements Driver: a tracking number and no label document.
func (Manual) CreateLabel(context.Context, domain.Shipment) (Label, error) {
	return Label{TrackingNumber: domain.NewTrackingNumber()}, nil
}

// Cancel implements Driver: there is no carrier to tell.
func (Manual) Cancel(context.Context, domain.Shipment) error { return nil }

// ManualUpdates implements Driver.
func (Manual) ManualUpdates() bool { return true }
