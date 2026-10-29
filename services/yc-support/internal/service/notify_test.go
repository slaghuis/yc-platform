package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/integration"
)

type recordingMessages struct {
	sent []integration.Send
	err  error
}

func (r *recordingMessages) Send(_ context.Context, s integration.Send) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, s)
	return nil
}

type fixedTenants struct {
	email string
	err   error
}

func (f fixedTenants) ContactEmail(context.Context, string) (string, error) { return f.email, f.err }

func newNotifier(msgs *recordingMessages, tenants Tenants) *Service {
	return &Service{
		outbound: msgs, tenants: tenants, now: time.Now,
		cfg: Config{IdleTTL: DefaultIdleTTL, StorefrontBase: ""},
	}
}

var (
	visitorReq = domain.Request{
		ID: "r1", TenantID: "shop-a", Reference: "SUP-AAAAAA", Topic: domain.TopicDelivery,
		ContactEmail: "dana@example.com", ContactName: "Dana", UserType: domain.UserVisitor,
	}
	customerReq = domain.Request{
		ID: "r2", TenantID: "shop-a", Reference: "SUP-BBBBBB", Topic: domain.TopicOrder,
		ContactEmail: "alice@example.com", ContactName: "Alice", UserID: "u1", UserType: domain.UserCustomer,
	}
)

func TestSupportURL(t *testing.T) {
	assert.Equal(t, "/account/support/abc", SupportURL("", "shop-a", "abc"))
	assert.Equal(t, "https://shop.test/account/support/abc", SupportURL("https://shop.test/", "shop-a", "abc"))
	assert.Equal(t, "https://shop-a.shops.test/account/support/abc", SupportURL("https://{tenant_id}.shops.test", "shop-a", "abc"))
	assert.Equal(t, "/account/support/a%2Fb", SupportURL("", "shop-a", "a/b"))
}

func TestRequesterMessageForVisitor(t *testing.T) {
	s := newNotifier(nil, nil)
	m, ok := s.requesterMessage(visitorReq, MsgAck, "created", map[string]any{"excerpt": "hi"})
	require.True(t, ok)
	assert.Equal(t, "support_ack", m.Type)
	assert.Equal(t, "email", m.Channel)
	assert.Equal(t, "dana@example.com", m.Address)
	assert.Empty(t, m.UserID)
	assert.Equal(t, "support:r1:created:customer", m.Reference)
	assert.NotContains(t, m.Context, "support_url")
	assert.Equal(t, map[string]any{
		"audience": "customer", "reference": "SUP-AAAAAA", "topic": "delivery", "request_id": "r1",
		"contact_name": "Dana", "excerpt": "hi",
	}, m.Context)
}

func TestRequesterMessageForCustomer(t *testing.T) {
	s := newNotifier(nil, nil)
	s.cfg.StorefrontBase = "https://{tenant_id}.shops.test"
	m, ok := s.requesterMessage(customerReq, MsgShopReply, "message:m1", nil)
	require.True(t, ok)
	assert.Equal(t, "u1", m.UserID, "addressed by account")
	assert.Empty(t, m.Channel)
	assert.Empty(t, m.Address)
	assert.Equal(t, "https://shop-a.shops.test/account/support/r2", m.Context["support_url"])
}

func TestRequesterMessageSkipsErasedRequests(t *testing.T) {
	erased := customerReq
	erased.UserID, erased.ContactEmail = "", ""
	at := time.Now()
	erased.RedactedAt = &at
	_, ok := newNotifier(nil, nil).requesterMessage(erased, MsgShopReply, "k", nil)
	assert.False(t, ok)
}

func TestShopMessage(t *testing.T) {
	m := newNotifier(nil, nil).shopMessage(customerReq, "shop@example.com", MsgNewRequest, "created", nil)
	assert.Equal(t, "support_new_request", m.Type)
	assert.Equal(t, "email", m.Channel)
	assert.Equal(t, "shop@example.com", m.Address)
	assert.Empty(t, m.UserID)
	assert.Equal(t, "shop", m.Context["audience"])
	assert.NotContains(t, m.Context, "support_url", "the shop's link is the admin UI, not the storefront")
	assert.Equal(t, "support:r2:created:shop", m.Reference)
}

func TestReferencesDifferByAudienceAndEvent(t *testing.T) {
	assert.NotEqual(t, reference("r", "closed", audienceRequester), reference("r", "closed", audienceShop))
	assert.NotEqual(t, reference("r", "message:1", audienceShop), reference("r", "message:2", audienceShop))
	assert.Equal(t, reference("r", "closed", audienceShop), reference("r", "closed", audienceShop), "retries repeat it")
}

func TestNotifyCreated(t *testing.T) {
	msgs := &recordingMessages{}
	s := newNotifier(msgs, fixedTenants{email: "shop@example.com"})
	s.notifyCreated(context.Background(), visitorReq, domain.Message{Body: "  Is it\nnut free? "})
	require.Len(t, msgs.sent, 2)
	assert.Equal(t, MsgAck, msgs.sent[0].Type)
	assert.Equal(t, "Is it nut free?", msgs.sent[0].Context["excerpt"])
	assert.Equal(t, MsgNewRequest, msgs.sent[1].Type)
	assert.Equal(t, "shop@example.com", msgs.sent[1].Address)
}

func TestNotifyPostedGoesToTheOtherSide(t *testing.T) {
	msgs := &recordingMessages{}
	s := newNotifier(msgs, fixedTenants{email: "shop@example.com"})
	ctx := context.Background()

	s.notifyPosted(ctx, customerReq, domain.Message{ID: "m1", AuthorType: domain.AuthorShopAdmin, Body: "answer"})
	require.Len(t, msgs.sent, 1)
	assert.Equal(t, MsgShopReply, msgs.sent[0].Type)
	assert.Equal(t, "u1", msgs.sent[0].UserID)

	msgs.sent = nil
	s.notifyPosted(ctx, customerReq, domain.Message{ID: "m2", AuthorType: domain.AuthorCustomer, Body: "reply"})
	require.Len(t, msgs.sent, 1)
	assert.Equal(t, MsgCustomerReply, msgs.sent[0].Type)
	assert.Equal(t, "shop@example.com", msgs.sent[0].Address)

	msgs.sent = nil
	s.notifyPosted(ctx, customerReq, domain.Message{ID: "m3", AuthorType: domain.AuthorSystem, Body: "closed"})
	assert.Empty(t, msgs.sent, "system messages are announced by the close notification")
}

func TestNotifyClosedTellsBoth(t *testing.T) {
	msgs := &recordingMessages{}
	s := newNotifier(msgs, fixedTenants{email: "shop@example.com"})
	closed := visitorReq
	closed.ClosedReason = domain.ClosedIdle
	s.notifyClosed(context.Background(), closed)
	require.Len(t, msgs.sent, 2)
	for _, m := range msgs.sent {
		assert.Equal(t, MsgClosed, m.Type)
		assert.Equal(t, "idle", m.Context["closed_reason"])
	}
	assert.Equal(t, "customer", msgs.sent[0].Context["audience"])
	assert.Equal(t, "shop", msgs.sent[1].Context["audience"])
}

func TestNotificationFailuresAreNotReturned(t *testing.T) {
	msgs := &recordingMessages{err: errors.New("yc-message down")}
	s := newNotifier(msgs, fixedTenants{email: "shop@example.com"})
	assert.NotPanics(t, func() {
		s.notifyCreated(context.Background(), visitorReq, domain.Message{Body: "x"})
	})
}

func TestShopNotificationNeedsContactEmail(t *testing.T) {
	msgs := &recordingMessages{}
	s := newNotifier(msgs, fixedTenants{err: integration.ErrNoContactEmail})
	s.notifyCreated(context.Background(), visitorReq, domain.Message{Body: "x"})
	require.Len(t, msgs.sent, 1, "the visitor is still acknowledged")
	assert.Equal(t, MsgAck, msgs.sent[0].Type)
}

func TestIdleClosedBody(t *testing.T) {
	assert.Equal(t, "This request was closed automatically after 72 hours without a message.", idleClosedBody(72*time.Hour))
	assert.Contains(t, idleClosedBody(24*time.Hour), "24 hours")
}

func TestRequestIDValidation(t *testing.T) {
	id, err := requestID("0B4C0E60-0F46-4E36-A1B0-7A6F5C8D9E10")
	require.NoError(t, err)
	assert.Equal(t, "0b4c0e60-0f46-4e36-a1b0-7a6f5c8d9e10", id)
	_, err = requestID("not-a-uuid")
	assert.Error(t, err)
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(nil, nil, nil, nil, Config{})
	assert.Equal(t, 72*time.Hour, s.cfg.IdleTTL)
	assert.Equal(t, 50, s.cfg.BatchSize)
}
