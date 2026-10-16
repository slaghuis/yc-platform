package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// SendRequest is another service's request to send a message.
type SendRequest struct {
	Type domain.Type
	// Reference makes the request idempotent: a repeated reference returns
	// the message already queued for it.
	Reference string
	UserID    string
	// Channel and Address, when given, are used instead of the user's
	// preferred channel.
	Channel domain.Channel
	Address string
	Locale  string
	Context map[string]any
}

// platformSendTypes are the messages Send accepts for the platform tenant:
// those for system admins, who belong to no shop.
var platformSendTypes = map[domain.Type]bool{
	domain.TypePasswordReset: true,
	domain.TypeAdminInvite:   true,
}

// Send queues a message for another service, such as a password reset
// from yc-identity, whose context must not travel on the event bus.
func (s *Service) Send(ctx context.Context, r SendRequest) (domain.Message, error) {
	tenantID := callerTenant(ctx)
	v := ycerrors.Validation()
	if !r.Type.Valid() {
		v.Field("type", "invalid", "Unknown message type")
	}
	if tenantID == eventbus.PlatformTenantID {
		if r.Type.Valid() && !platformSendTypes[r.Type] {
			v.Field("type", "invalid", "Messages of type "+string(r.Type)+" belong to a shop")
		}
		// System admins have no preferences to resolve an address from.
		if r.Address == "" {
			v.Field("address", "required", "An address is required for the platform")
		}
	}
	if r.Reference == "" {
		v.Field("reference", "required", "Reference is required")
	}
	if r.Address == "" && r.UserID == "" {
		v.Field("address", "required", "Address or user ID is required")
	}
	if r.Address != "" && r.Channel == "" {
		v.Field("channel", "required", "Channel is required with an address")
	}
	if r.Locale != "" && !domain.ValidLocale(r.Locale) {
		v.Field("locale", "invalid", "Locale is a language tag such as en or en-ZA")
	}
	if v.HasErrors() {
		return domain.Message{}, v.Build()
	}
	to := domain.Recipient{UserID: r.UserID, Channel: r.Channel, Address: r.Address, Locale: r.Locale}
	if r.Address == "" {
		resolved, ok, err := s.customer(ctx, r.UserID)
		if err != nil {
			return domain.Message{}, err
		}
		if !ok {
			return domain.Message{}, ycerrors.Conflict("The user has no address to send to")
		}
		to = resolved
		if r.Locale != "" {
			to.Locale = r.Locale
		}
	}
	var m domain.Message
	err := s.inAnyTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		var err error
		m, err = s.queue(ctx, tx, tenantID, request{Type: r.Type, Reference: "send:" + r.Reference, To: to, Context: r.Context})
		return err
	})
	m.Redact()
	return m, err
}
