//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-message/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorID, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   actorID,
		authctx.HeaderActorType: actorType,
	}}
}

// service is another service calling through the gateway's internal
// listener.
func service(t *testing.T, srv *httptest.Server, tenant, name string) client {
	c := as(t, srv, tenant, name, "service")
	c.hdr[authctx.HeaderCallerService] = name
	return c
}

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, &buf)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	if resp.StatusCode >= 400 {
		problem, _ := io.ReadAll(resp.Body)
		c.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, problem)
	}
	return resp.StatusCode
}

const shop = "shop-a"

func TestMessaging(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-message")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	box := newMailbox(t)
	up := newUpstreams()
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_MESSAGE_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gw.URL)
	t.Setenv("YC_MESSAGE_SERVICE_SECRET", "message-secret")
	t.Setenv("SMTP_ADDR", box.Addr())
	t.Setenv("YC_MESSAGE_SHOP_URL", "https://{tenant}.shops.test")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	cfg.Messages.RetryBackoff = 20 * time.Millisecond
	cfg.Messages.DispatchInterval = 50 * time.Millisecond
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-message", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	outbox := func(eventType string) []map[string]any {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT envelope->'payload' FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2 ORDER BY created_at, id`, shop, eventType)
		require.NoError(t, err)
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var p map[string]any
			require.NoError(t, rows.Scan(&p))
			out = append(out, p)
		}
		require.NoError(t, rows.Err())
		return out
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-test", Validator: validator,
		Streams: []eventbus.StreamConfig{
			eventbus.IdentityStream(), eventbus.OrderStream(), eventbus.CartStream(),
			eventbus.ReviewStream(), eventbus.PreferenceStream(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	publish := func(payload interface{ EventType() string }) {
		t.Helper()
		require.NoError(t, bus.PublishDirect(ctx, eventbus.Event{
			EventType: payload.EventType(), TenantID: shop,
			Actor: eventbus.Actor{Type: "service", ID: "yc-test"}, Payload: payload,
		}))
	}
	waitMail := func(addr string, n int) []sentMail {
		t.Helper()
		require.Eventually(t, func() bool { return len(box.to(addr)) >= n }, 10*time.Second, 20*time.Millisecond,
			"%d mails to %s", n, addr)
		return box.to(addr)
	}

	admin := as(t, srv, shop, uuid.NewString(), "shop_admin")
	customer := as(t, srv, shop, uuid.NewString(), "customer")
	identity := service(t, srv, shop, "yc-identity")
	preference := service(t, srv, shop, "yc-preference")
	zar := func(minor int64) events.Money { return events.Money{AmountMinor: minor, Currency: "ZAR"} }

	thandi := uuid.NewString()
	up.set(func(u *upstreams) {
		u.preferences[thandi] = map[string]any{
			"user_id": thandi, "locale": "en-ZA", "primary_channel": "email",
			"channels": []map[string]any{{"channel": "email", "address": "thandi@x.test", "verified": false}},
		}
	})

	t.Run("welcome email on registration", func(t *testing.T) {
		name := "Thandi"
		publish(events.IdentityUserRegisteredV1{
			UserID: thandi, Email: "thandi@x.test", DisplayName: &name, Role: "customer", Method: "password",
			RegisteredAt: time.Now().UTC(),
		})
		m := waitMail("thandi@x.test", 1)[0]
		assert.Equal(t, "Welcome to the shop", m.Subject)
		assert.Contains(t, m.Body, "Hi Thandi")
		assert.Contains(t, m.Body, "https://shop-a.shops.test")
		require.Eventually(t, func() bool { return len(outbox(events.TypeMessageSentV1)) == 1 }, 5*time.Second, 20*time.Millisecond)
		sent := outbox(events.TypeMessageSentV1)[0]
		assert.Equal(t, "welcome", sent["message_type"])
		assert.Equal(t, "smtp", sent["driver"])
	})

	orderID := uuid.NewString()
	t.Run("order confirmation for a customer", func(t *testing.T) {
		publish(events.OrderPlacedV1{
			OrderID: orderID, OrderNumber: "YC-1001", UserID: thandi, UserType: "customer",
			CheckoutSessionID: uuid.NewString(), RequiresShipping: true,
			Items: []events.OrderPlacedV1ItemsItem{{
				SKUID: uuid.NewString(), SKUCode: "MUG-1", NameSnapshot: "Blue mug", Quantity: 2,
				UnitPrice: zar(12995), LineTotal: zar(25990),
			}},
			ItemsTotal: zar(25990), Shipping: zar(9900), TaxTotal: zar(4681), Total: zar(35890), PlacedAt: time.Now().UTC(),
		})
		m := waitMail("thandi@x.test", 2)[1]
		assert.Equal(t, "Order YC-1001 confirmed", m.Subject)
		assert.Contains(t, m.Body, "2 x Blue mug")
		assert.Contains(t, m.Body, "ZAR 259.90")
		assert.Contains(t, m.Body, "ZAR 358.90")
		assert.Contains(t, m.Body, "https://shop-a.shops.test/orders/"+orderID)
	})

	t.Run("guest order goes to the guest contact", func(t *testing.T) {
		guestOrder := uuid.NewString()
		publish(events.OrderPlacedV1{
			OrderID: guestOrder, OrderNumber: "YC-1002", UserID: uuid.NewString(), UserType: "visitor",
			CheckoutSessionID: uuid.NewString(),
			Items: []events.OrderPlacedV1ItemsItem{{
				SKUID: uuid.NewString(), SKUCode: "EBOOK", NameSnapshot: "Cookbook", Quantity: 1,
				UnitPrice: zar(9900), LineTotal: zar(9900),
			}},
			ItemsTotal: zar(9900), Shipping: zar(0), TaxTotal: zar(1291), Total: zar(9900), PlacedAt: time.Now().UTC(),
			GuestContact: &events.GuestContact{Email: "guest@x.test", PhoneE164: "+27820000002"},
		})
		m := waitMail("guest@x.test", 1)[0]
		assert.Equal(t, "Order YC-1002 confirmed", m.Subject)
		assert.NotContains(t, m.Body, "Shipping:", "digital order")
	})

	t.Run("shipped email reads the order", func(t *testing.T) {
		up.set(func(u *upstreams) {
			u.orders[orderID] = map[string]any{
				"order_id": orderID, "order_number": "YC-1001", "user_id": thandi, "user_type": "customer",
			}
		})
		carrier := "Courier Guy"
		publish(events.OrderShippedV1{
			OrderID: orderID, OrderNumber: "YC-1001", UserID: thandi, TrackingNumber: "YC-TRK-1",
			CarrierName: &carrier, ShippedAt: time.Now().UTC(),
		})
		m := waitMail("thandi@x.test", 3)[2]
		assert.Equal(t, "Order YC-1001 is on its way", m.Subject)
		assert.Contains(t, m.Body, "YC-TRK-1")
		assert.Contains(t, m.Body, "Courier Guy")
		assert.Contains(t, m.Body, "https://shop-a.shops.test/shipping/tracking/YC-TRK-1")
	})

	t.Run("redelivered events send once", func(t *testing.T) {
		before := len(box.to("thandi@x.test"))
		require.NoError(t, bus.PublishDirect(ctx, eventbus.Event{
			EventType: events.TypeOrderDeliveredV1, TenantID: shop, Actor: eventbus.Actor{Type: "service", ID: "yc-test"},
			Payload: events.OrderDeliveredV1{
				OrderID: orderID, OrderNumber: "YC-1001", UserID: thandi, DeliveredAt: time.Now().UTC(),
				Items: []events.OrderDeliveredV1ItemsItem{{SKUID: uuid.NewString(), Quantity: 2}},
			},
		}))
		waitMail("thandi@x.test", before+1)
		var list api.MessageList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/messages?type=order_delivered", nil, &list))
		require.Len(t, list.Items, 1)
		ref := list.Items[0].Reference
		assert.Regexp(t, `^event:[0-9a-f-]+:order_delivered$`, ref)
	})

	t.Run("abandoned cart and review prompt resolve the customer", func(t *testing.T) {
		before := len(box.to("thandi@x.test"))
		publish(events.CartAbandonedV1{
			CartID: uuid.NewString(), UserID: thandi, CartType: "default", ItemCount: 2, Quantity: 3,
			LastActivityAt: time.Now().Add(-time.Hour).UTC(), AbandonedAt: time.Now().UTC(),
		})
		m := waitMail("thandi@x.test", before+1)[before]
		assert.Contains(t, m.Body, "https://shop-a.shops.test/cart")

		productID := uuid.NewString()
		publish(events.ReviewPromptRequestedV1{
			OrderID: orderID, OrderNumber: "YC-1001", UserID: thandi, RequestedAt: time.Now().UTC(),
			Products: []events.ReviewPromptRequestedV1ProductsItem{{ProductID: productID, Name: "Blue mug"}},
		})
		m = waitMail("thandi@x.test", before+2)[before+1]
		assert.Contains(t, m.Body, "Blue mug")
		assert.Contains(t, m.Body, "https://shop-a.shops.test/products/"+productID+"#review")
	})

	t.Run("password reset through the internal send is redacted for admins", func(t *testing.T) {
		req := api.SendRequest{
			Type: "password_reset", Reference: "reset:" + uuid.NewString(), UserID: ptr(uuid.MustParse(thandi)),
			Channel: ptr(api.Channel("email")), Address: ptr("thandi@x.test"),
			Context: &map[string]interface{}{"reset_url": "https://shop-a.shops.test/reset?token=s3cret", "expires_at": "12:00 UTC"},
		}
		var r1, r2 api.MessageReceipt
		require.Equal(t, http.StatusAccepted, identity.do("POST", "/internal/send", req, &r1))
		require.Equal(t, http.StatusAccepted, identity.do("POST", "/internal/send", req, &r2))
		assert.Equal(t, r1.MessageID, r2.MessageID, "same reference, same message")

		require.Eventually(t, func() bool {
			for _, m := range box.to("thandi@x.test") {
				if m.Subject == "Reset your password" {
					return assert.Contains(t, m.Body, "token=s3cret")
				}
			}
			return false
		}, 10*time.Second, 20*time.Millisecond)
		var got api.Message
		require.Eventually(t, func() bool {
			require.Equal(t, http.StatusOK, admin.do("GET", "/admin/messages/"+r1.MessageID.String(), nil, &got))
			return got.Status == "sent"
		}, 5*time.Second, 20*time.Millisecond)
		assert.Equal(t, "[redacted]", got.Body)

		missing := req
		missing.Context = &map[string]interface{}{"expires_at": "12:00 UTC"}
		missing.Reference = "reset:" + uuid.NewString()
		assert.Equal(t, http.StatusBadRequest, identity.do("POST", "/internal/send", missing, nil), "template needs reset_url")
		assert.Equal(t, http.StatusForbidden, customer.do("GET", "/admin/messages", nil, nil))
	})

	t.Run("shop templates override the defaults", func(t *testing.T) {
		path := "/admin/templates/welcome/email/en"
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", path, api.TemplateInput{
			Subject: ptr("Hi"), Body: "Hello {{.full_name}}",
		}, nil), "unknown field")
		var tpl api.Template
		require.Equal(t, http.StatusOK, admin.do("PUT", path, api.TemplateInput{
			Subject: ptr("Welcome to Mugs & Co, {{.name}}"), Body: "<p>Hi {{.name}}</p>", Format: ptr(api.Format("html")),
		}, &tpl))
		assert.False(t, tpl.Platform)
		assert.Equal(t, 1, tpl.Version)

		var effective api.Template
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/templates/welcome/email/en-ZA", nil, &effective))
		assert.Equal(t, "Welcome to Mugs & Co, {{.name}}", deref(effective.Subject), "en-ZA falls back to en")

		var list api.TemplateList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/templates", nil, &list))
		assert.Greater(t, len(list.Items), 20)

		var test api.Message
		require.Equal(t, http.StatusAccepted, admin.do("POST", path+"/test", map[string]string{"address": "owner@x.test"}, &test))
		m := waitMail("owner@x.test", 1)[0]
		assert.Equal(t, "Welcome to Mugs & Co, Thandi", m.Subject)
		assert.Contains(t, m.Body, "<p>Hi Thandi</p>")

		require.Equal(t, http.StatusNoContent, admin.do("DELETE", path, nil, nil))
		require.Equal(t, http.StatusOK, admin.do("GET", path, nil, &effective))
		assert.True(t, effective.Platform)
		assert.Equal(t, http.StatusNotFound, admin.do("DELETE", path, nil, nil))
	})

	sipho := uuid.NewString()
	codeRE := regexp.MustCompile(`code is (\d{6})`)
	var smsProviderID string
	t.Run("channel verification by SMS", func(t *testing.T) {
		up.set(func(u *upstreams) {
			u.preferences[sipho] = map[string]any{
				"user_id": sipho, "locale": "en", "primary_channel": "email",
				"channels": []map[string]any{{"channel": "email", "address": "sipho@x.test", "verified": true}},
			}
		})
		publish(events.PreferenceChannelAddedV1{UserID: sipho, Channel: "sms", Address: "+27820000003", AddedAt: time.Now().UTC()})
		m := waitMail("27820000003@sms.test", 1)[0]
		assert.Equal(t, "SMS to +27820000003", m.Subject)
		match := codeRE.FindStringSubmatch(m.Body)
		require.Len(t, match, 2)
		code := match[1]

		addr := api.ChannelAddress{UserID: uuid.MustParse(sipho), Channel: "sms", Address: "+27820000003"}
		assert.Equal(t, http.StatusTooManyRequests, preference.do("POST", "/internal/channels/verify-start", addr, nil),
			"resend within a minute")

		confirm := func(code, address string) int {
			return preference.do("POST", "/internal/channels/verify-confirm", map[string]any{
				"user_id": sipho, "channel": "sms", "address": address, "code": code,
			}, nil)
		}
		wrong := "000000"
		if code == wrong {
			wrong = "111111"
		}
		assert.Equal(t, http.StatusBadRequest, confirm(wrong, "+27820000003"))
		assert.Equal(t, http.StatusConflict, confirm(code, "+27820000009"), "another address")
		assert.Equal(t, http.StatusOK, confirm(code, "+27820000003"))
		assert.Equal(t, http.StatusOK, confirm(code, "+27820000003"), "again")
		confirmed := outbox(events.TypeMessageVerificationConfirmedV1)
		require.Len(t, confirmed, 1)
		assert.Equal(t, "+27820000003", confirmed[0]["address"])
		require.Len(t, outbox(events.TypeMessageVerificationSentV1), 1)

		var list api.MessageList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/messages?type=channel_verification", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, "[redacted]", list.Items[0].Body, "admins never see codes")
		assert.Equal(t, "fake-sms", list.Items[0].Driver)
		smsProviderID = deref(list.Items[0].ProviderMessageID)
	})

	t.Run("a verified primary SMS channel is used, and bounces are reported", func(t *testing.T) {
		up.set(func(u *upstreams) {
			u.preferences[sipho]["primary_channel"] = "sms"
			u.preferences[sipho]["channels"] = []map[string]any{
				{"channel": "email", "address": "sipho@x.test", "verified": true},
				{"channel": "sms", "address": "+27820000003", "verified": true},
			}
		})
		publish(events.CartAbandonedV1{
			CartID: uuid.NewString(), UserID: sipho, CartType: "default", ItemCount: 1, Quantity: 1,
			LastActivityAt: time.Now().UTC(), AbandonedAt: time.Now().UTC(),
		})
		waitMail("27820000003@sms.test", 2)
		assert.Empty(t, box.to("sipho@x.test"))

		webhook := as(t, srv, shop, "", "visitor")
		require.NotEmpty(t, smsProviderID)
		assert.Equal(t, http.StatusNoContent, webhook.do("POST", "/webhooks/fake-sms", map[string]any{
			"provider_message_id": smsProviderID, "status": "bounced", "reason": "number unreachable",
		}, nil))
		assert.Equal(t, http.StatusNoContent, webhook.do("POST", "/webhooks/fake-sms", map[string]any{
			"provider_message_id": smsProviderID, "status": "bounced",
		}, nil), "repeat")
		bounced := outbox(events.TypeMessageBouncedV1)
		require.Len(t, bounced, 1)
		assert.Equal(t, "+27820000003", bounced[0]["address"])
		assert.Equal(t, "number unreachable", bounced[0]["reason"])
		assert.Equal(t, http.StatusNotFound, webhook.do("POST", "/webhooks/fake-sms", map[string]any{
			"provider_message_id": "nope", "status": "delivered",
		}, nil))
	})

	t.Run("temporary failures retry; permanent ones fail", func(t *testing.T) {
		box.mu.Lock()
		box.tempfail["flaky@x.test"] = 2
		box.reject["gone@x.test"] = true
		box.mu.Unlock()
		send := func(addr string) api.MessageReceipt {
			var r api.MessageReceipt
			require.Equal(t, http.StatusAccepted, identity.do("POST", "/internal/send", api.SendRequest{
				Type: "welcome", Reference: uuid.NewString(), Channel: ptr(api.Channel("email")), Address: ptr(addr),
				Context: &map[string]interface{}{"name": "Test"},
			}, &r))
			return r
		}
		flaky, gone := send("flaky@x.test"), send("gone@x.test")
		waitMail("flaky@x.test", 1)
		var m api.Message
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/messages/"+flaky.MessageID.String(), nil, &m))
		assert.Equal(t, 3, m.Attempts)

		require.Eventually(t, func() bool {
			admin.do("GET", "/admin/messages/"+gone.MessageID.String(), nil, &m)
			return m.Status == "failed"
		}, 5*time.Second, 20*time.Millisecond)
		assert.Equal(t, 1, m.Attempts, "a 5xx is not retried")
		assert.Contains(t, deref(m.LastError), "550")
		failed := outbox(events.TypeMessageFailedV1)
		require.Len(t, failed, 1)
		assert.Equal(t, gone.MessageID.String(), failed[0]["message_id"])
	})

	t.Run("customers without an address are skipped", func(t *testing.T) {
		publish(events.CartAbandonedV1{
			CartID: uuid.NewString(), UserID: uuid.NewString(), CartType: "default", ItemCount: 1, Quantity: 1,
			LastActivityAt: time.Now().UTC(), AbandonedAt: time.Now().UTC(),
		})
		time.Sleep(300 * time.Millisecond)
		var list api.MessageList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/messages?type=cart_abandoned", nil, &list))
		assert.Len(t, list.Items, 2)
	})
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
