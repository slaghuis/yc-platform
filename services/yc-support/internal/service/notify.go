package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/integration"
)

// yc-message type names for support notifications. The templates are added
// to yc-message separately; until they exist yc-message answers 400 and
// the notification is logged and dropped.
const (
	// MsgAck acknowledges a new request to the requester, with its reference.
	MsgAck = "support_ack"
	// MsgShopReply tells the requester the shop answered.
	MsgShopReply = "support_shop_reply"
	// MsgClosed tells the requester and the shop a request closed; the
	// context's audience says which.
	MsgClosed = "support_closed"
	// MsgNewRequest tells the shop a request was opened.
	MsgNewRequest = "support_new_request"
	// MsgCustomerReply tells the shop the requester replied.
	MsgCustomerReply = "support_customer_reply"
)

// Who a notification is for, in the template context's "audience".
const (
	audienceRequester = "customer"
	audienceShop      = "shop"
)

// SupportURL is the customer's link to a thread, /account/support/{id},
// under base when the shop's storefront is configured.
func SupportURL(base, tenantID, requestID string) string {
	base = strings.ReplaceAll(strings.TrimRight(base, "/"), "{tenant_id}", url.PathEscape(tenantID))
	return base + "/account/support/" + url.PathEscape(requestID)
}

// requesterMessage builds the message to a request's customer or visitor.
// A customer (user_id set) is addressed by account, so yc-message picks
// their channel (ADR-023), and gets the thread link; a visitor gets email
// at the contact address and no link. It reports false when nobody can be
// written to.
func (s *Service) requesterMessage(r domain.Request, typ, key string, extra map[string]any) (integration.Send, bool) {
	if !r.Notifiable() {
		return integration.Send{}, false
	}
	m := integration.Send{
		Type:      typ,
		Reference: reference(r.ID, key, audienceRequester),
		Context:   s.context(r, audienceRequester, extra),
	}
	if r.UserID != "" {
		m.UserID = r.UserID
		m.Context["support_url"] = SupportURL(s.cfg.StorefrontBase, r.TenantID, r.ID)
	} else {
		m.Channel, m.Address = "email", r.ContactEmail
	}
	return m, true
}

// shopMessage builds the message to the shop's contact email.
func (s *Service) shopMessage(r domain.Request, email, typ, key string, extra map[string]any) integration.Send {
	return integration.Send{
		Type:      typ,
		Reference: reference(r.ID, key, audienceShop),
		Channel:   "email",
		Address:   email,
		Context:   s.context(r, audienceShop, extra),
	}
}

func (s *Service) context(r domain.Request, audience string, extra map[string]any) map[string]any {
	c := map[string]any{
		"audience":     audience,
		"reference":    r.Reference,
		"topic":        string(r.Topic),
		"request_id":   r.ID,
		"contact_name": r.ContactName,
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// reference makes a message's idempotency key: the same event to the same
// audience is queued once however often it is retried.
func reference(requestID, key, audience string) string {
	return fmt.Sprintf("support:%s:%s:%s", requestID, key, audience)
}

// notifyCreated acknowledges a new request to the requester and tells the
// shop.
func (s *Service) notifyCreated(ctx context.Context, r domain.Request, first domain.Message) {
	extra := map[string]any{"excerpt": domain.Excerpt(first.Body)}
	if m, ok := s.requesterMessage(r, MsgAck, "created", extra); ok {
		s.send(ctx, m)
	}
	s.sendToShop(ctx, r, MsgNewRequest, "created", extra)
}

// notifyPosted tells the other side about a message: the requester of a
// shop reply, the shop of a requester's.
func (s *Service) notifyPosted(ctx context.Context, r domain.Request, msg domain.Message) {
	extra := map[string]any{"excerpt": domain.Excerpt(msg.Body)}
	key := "message:" + msg.ID
	switch {
	case msg.AuthorType == domain.AuthorShopAdmin:
		if m, ok := s.requesterMessage(r, MsgShopReply, key, extra); ok {
			s.send(ctx, m)
		}
	case msg.AuthorType.FromRequester():
		s.sendToShop(ctx, r, MsgCustomerReply, key, extra)
	}
}

// notifyClosed tells the requester and the shop that a request closed and
// why.
func (s *Service) notifyClosed(ctx context.Context, r domain.Request) {
	extra := map[string]any{"closed_reason": string(r.ClosedReason)}
	if m, ok := s.requesterMessage(r, MsgClosed, "closed", extra); ok {
		s.send(ctx, m)
	}
	s.sendToShop(ctx, r, MsgClosed, "closed", extra)
}

func (s *Service) sendToShop(ctx context.Context, r domain.Request, typ, key string, extra map[string]any) {
	email, err := s.tenants.ContactEmail(ctx, r.TenantID)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Warn("support: shop contact email unavailable, shop not notified",
			"type", typ, "request_id", r.ID, "err", err)
		return
	}
	s.send(ctx, s.shopMessage(r, email, typ, key, extra))
}

// send hands m to yc-message. The change it announces is committed, so a
// failure is logged rather than returned.
func (s *Service) send(ctx context.Context, m integration.Send) {
	if err := s.outbound.Send(ctx, m); err != nil {
		telemetry.LoggerFromContext(ctx).Warn("support: notification not sent",
			"type", m.Type, "reference", m.Reference, "err", err)
	}
}
