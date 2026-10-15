//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	api "github.com/slaghuis/yc-platform/services/yc-preference/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/app"
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
	return resp.StatusCode
}

func TestPreference(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-preference")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	messages := &fakeMessages{}
	gw := httptest.NewServer(messages)
	t.Cleanup(gw.Close)
	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_PREFERENCE_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gw.URL)
	t.Setenv("YC_PREFERENCE_SERVICE_SECRET", "preference-secret")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-preference", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	// outbox returns the payloads of tenant's eventType events, oldest
	// first.
	outbox := func(tenant, eventType string) []map[string]any {
		rows, err := db.Query(ctx, `SELECT envelope->'payload' FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2 ORDER BY created_at, id`, tenant, eventType)
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
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-identity", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.PlatformStream(), eventbus.IdentityStream(), eventbus.MessageStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })
	publish := func(tenant, eventType string, payload any) {
		t.Helper()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: eventType, TenantID: tenant,
			Actor: eventbus.Actor{Type: "service", ID: "yc-identity"}, Payload: payload,
		}))
	}
	register := func(tenant, userID, email string) {
		t.Helper()
		publish(tenant, events.TypeIdentityUserRegisteredV1, events.IdentityUserRegisteredV1{
			UserID: userID, Email: email, Role: "customer", Method: "password", RegisteredAt: time.Now().UTC(),
		})
	}

	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	alice, bob := uuid.NewString(), uuid.NewString()
	aliceC := as(t, srv, "shop-a", alice, "customer")

	t.Run("a provisioned shop defaults to its store currency", func(t *testing.T) {
		publish(eventbus.PlatformTenantID, events.TypePlatformTenantProvisionedV1, events.PlatformTenantProvisionedV1{
			TenantID: "shop-a", Name: "Shop A", Hostnames: []string{"shop-a.test"},
			StoreCurrency: "USD", DefaultJurisdiction: "US", IsTaxInclusive: false, ProvisionedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			var d api.Defaults
			return admin.do("GET", "/admin/preferences/defaults", nil, &d) == http.StatusOK && d.DisplayCurrency == "USD"
		}, 10*time.Second, 50*time.Millisecond)
	})

	t.Run("registration seeds preferences once", func(t *testing.T) {
		register("shop-a", alice, "Alice@Example.com")
		register("shop-a", alice, "alice@example.com")
		register("shop-a", bob, "bob@example.com")
		require.Eventually(t, func() bool {
			var p api.Preferences
			return as(t, srv, "shop-a", bob, "customer").do("GET", "/preferences/me", nil, &p) == http.StatusOK && p.Stored
		}, 10*time.Second, 50*time.Millisecond, "events are handled in order, so alice's duplicate is done too")

		var p api.Preferences
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/preferences/me", nil, &p))
		assert.Equal(t, "USD", p.DisplayCurrency)
		assert.Equal(t, "en-ZA", p.Locale)
		require.NotNil(t, p.PrimaryChannel)
		assert.Equal(t, api.Channel("email"), *p.PrimaryChannel)
		require.Len(t, p.Channels, 1)
		assert.Equal(t, "alice@example.com", p.Channels[0].Address)
		assert.False(t, p.Channels[0].Verified)

		assert.Len(t, outbox("shop-a", events.TypePreferenceCurrencyChangedV1), 2, "one per user")
		assert.Len(t, outbox("shop-a", events.TypePreferenceChannelAddedV1), 2)
		assert.Len(t, outbox("shop-a", events.TypePreferenceChannelChangedV1), 2)
		seeded := outbox("shop-a", events.TypePreferenceCurrencyChangedV1)[0]
		assert.Equal(t, "USD", seeded["display_currency"])
		assert.NotContains(t, seeded, "previous_display_currency")
	})

	t.Run("customer changes display currency and locale", func(t *testing.T) {
		var p api.Preferences
		require.Equal(t, http.StatusOK, aliceC.do("PATCH", "/preferences/me",
			map[string]any{"display_currency": "EUR", "locale": "de-DE"}, &p))
		assert.Equal(t, "EUR", p.DisplayCurrency)
		require.Equal(t, http.StatusOK, aliceC.do("PATCH", "/preferences/me", map[string]any{"display_currency": "EUR"}, nil), "unchanged")

		changes := outbox("shop-a", events.TypePreferenceCurrencyChangedV1)
		require.Len(t, changes, 3)
		assert.Equal(t, map[string]any{
			"user_id": alice, "display_currency": "EUR", "previous_display_currency": "USD",
			"changed_at": changes[2]["changed_at"],
		}, changes[2])
		assert.Len(t, outbox("shop-a", events.TypePreferenceLocaleChangedV1), 1)

		assert.Equal(t, http.StatusBadRequest, aliceC.do("PATCH", "/preferences/me", map[string]any{"display_currency": "euro"}, nil))
	})

	t.Run("customer manages messaging channels", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, aliceC.do("PATCH", "/preferences/me", map[string]any{"primary_channel": "sms"}, nil),
			"not a channel yet")

		sms := map[string]any{"channel": "sms", "address": "+27831234567"}
		var c api.MessagingChannel
		require.Equal(t, http.StatusCreated, aliceC.do("POST", "/preferences/me/channels", sms, &c))
		assert.Equal(t, "+27831234567", c.Address)
		require.Equal(t, http.StatusOK, aliceC.do("POST", "/preferences/me/channels", sms, nil), "unchanged")
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/preferences/me/channels",
			map[string]any{"channel": "sms", "address": "0831234567"}, nil))

		require.Equal(t, http.StatusOK, aliceC.do("PATCH", "/preferences/me", map[string]any{"primary_channel": "sms"}, nil))
		assert.Equal(t, http.StatusConflict, aliceC.do("DELETE", "/preferences/me/channels/sms", nil, nil), "primary")
		require.Equal(t, http.StatusOK, aliceC.do("PATCH", "/preferences/me", map[string]any{"primary_channel": "email"}, nil))
		require.Equal(t, http.StatusNoContent, aliceC.do("DELETE", "/preferences/me/channels/sms", nil, nil))
		assert.Equal(t, http.StatusNotFound, aliceC.do("DELETE", "/preferences/me/channels/sms", nil, nil))

		var list api.ChannelList
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/preferences/me/channels", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, api.Channel("email"), list.Items[0].Channel)
		assert.Len(t, outbox("shop-a", events.TypePreferenceChannelRemovedV1), 1)
		assert.Len(t, outbox("shop-a", events.TypePreferenceChannelChangedV1), 4)
	})

	t.Run("users without stored preferences see the shop defaults", func(t *testing.T) {
		var d api.Defaults
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/preferences/defaults",
			map[string]any{"display_currency": "GBP", "locale": "en-GB"}, &d))
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/preferences/defaults",
			map[string]any{"display_currency": "GBP", "locale": ""}, nil))

		var p api.Preferences
		require.Equal(t, http.StatusOK, as(t, srv, "shop-a", uuid.NewString(), "customer").do("GET", "/preferences/me", nil, &p))
		assert.False(t, p.Stored)
		assert.Equal(t, "GBP", p.DisplayCurrency)
		assert.Empty(t, p.Channels)

		require.Equal(t, http.StatusOK, aliceC.do("GET", "/preferences/me", nil, &p))
		assert.Equal(t, "EUR", p.DisplayCurrency, "stored preferences keep their values")

		var other api.Preferences
		require.Equal(t, http.StatusOK, as(t, srv, "shop-b", alice, "customer").do("GET", "/preferences/me", nil, &other))
		assert.False(t, other.Stored, "preferences belong to one shop")
		assert.Equal(t, "ZAR", other.DisplayCurrency, "the platform fallback")
	})

	t.Run("services read a user's preferences", func(t *testing.T) {
		svc := as(t, srv, "shop-a", "yc-message", "service")
		svc.hdr[authctx.HeaderCallerService] = "yc-message"
		var p api.Preferences
		require.Equal(t, http.StatusOK, svc.do("GET", "/internal/preferences/"+alice, nil, &p))
		assert.Equal(t, "EUR", p.DisplayCurrency)
		require.NotNil(t, p.PrimaryChannel)
		assert.Equal(t, http.StatusNotFound, svc.do("GET", "/internal/preferences/"+uuid.NewString(), nil, nil))
		assert.Equal(t, http.StatusUnauthorized, aliceC.do("GET", "/internal/preferences/"+alice, nil, nil), "services only")
	})

	t.Run("customers verify channels with yc-message's codes", func(t *testing.T) {
		var resent struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		require.Equal(t, http.StatusAccepted, aliceC.do("POST", "/preferences/me/channels/email/resend", nil, &resent))
		assert.WithinDuration(t, time.Now().Add(15*time.Minute), resent.ExpiresAt, time.Minute)
		assert.Equal(t, 1, messages.startCount())
		assert.Equal(t, http.StatusNotFound, aliceC.do("POST", "/preferences/me/channels/sms/resend", nil, nil))
		require.Equal(t, http.StatusCreated, aliceC.do("POST", "/preferences/me/channels",
			map[string]any{"channel": "telegram", "address": "@alice_shop"}, nil))
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/preferences/me/channels/telegram/resend", nil, nil),
			"no telegram codes yet")

		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/preferences/me/channels/email/verify",
			map[string]any{"code": "000000"}, nil))
		var c api.MessagingChannel
		require.Equal(t, http.StatusOK, aliceC.do("POST", "/preferences/me/channels/email/verify",
			map[string]any{"code": goodCode}, &c))
		assert.True(t, c.Verified)
		require.NotNil(t, c.VerifiedAt)
		require.Equal(t, http.StatusOK, aliceC.do("POST", "/preferences/me/channels/email/verify",
			map[string]any{"code": "anything"}, &c), "already verified")
		assert.Equal(t, http.StatusConflict, aliceC.do("POST", "/preferences/me/channels/email/resend", nil, nil))

		verified := outbox("shop-a", events.TypePreferenceChannelVerifiedV1)
		require.Len(t, verified, 1)
		assert.Equal(t, "alice@example.com", verified[0]["address"])
	})

	t.Run("confirmations and bounces from yc-message", func(t *testing.T) {
		bobC := as(t, srv, "shop-a", bob, "customer")
		require.Equal(t, http.StatusCreated, bobC.do("POST", "/preferences/me/channels",
			map[string]any{"channel": "sms", "address": "+27830000001"}, nil))
		publish("shop-a", events.TypeMessageVerificationConfirmedV1, events.MessageVerificationConfirmedV1{
			VerificationID: uuid.NewString(), UserID: bob, Channel: "sms", Address: "+27830000001", ConfirmedAt: time.Now().UTC(),
		})
		publish("shop-a", events.TypeMessageVerificationConfirmedV1, events.MessageVerificationConfirmedV1{
			VerificationID: uuid.NewString(), UserID: bob, Channel: "sms", Address: "+27839999999", ConfirmedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePreferenceChannelVerifiedV1)) == 2 },
			10*time.Second, 50*time.Millisecond)
		require.Equal(t, http.StatusOK, bobC.do("PATCH", "/preferences/me", map[string]any{"primary_channel": "sms"}, nil))
		changed := len(outbox("shop-a", events.TypePreferenceChannelChangedV1))

		publish("shop-a", events.TypeMessageBouncedV1, events.MessageBouncedV1{
			MessageID: uuid.NewString(), MessageType: "cart_abandoned", Channel: "sms", UserID: &bob,
			Address: "+27830000001", Reason: "unreachable", BouncedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePreferenceChannelChangedV1)) == changed+1 },
			10*time.Second, 50*time.Millisecond)
		var p api.Preferences
		require.Equal(t, http.StatusOK, bobC.do("GET", "/preferences/me", nil, &p))
		assert.Equal(t, api.Channel("email"), *p.PrimaryChannel, "falls back to email")
		for _, c := range p.Channels {
			if c.Channel == "sms" {
				assert.False(t, c.Verified)
			}
		}
	})

	t.Run("a deleted user's preferences are deleted (GDPR)", func(t *testing.T) {
		svc := as(t, srv, "shop-a", "yc-message", "service")
		svc.hdr[authctx.HeaderCallerService] = "yc-message"
		require.Equal(t, http.StatusOK, svc.do("GET", "/internal/preferences/"+bob, nil, nil))
		publish("shop-a", events.TypeIdentityUserDeletedV1, events.IdentityUserDeletedV1{
			UserID: bob, Pseudonym: uuid.NewString(), Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		done := outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)[0]
		assert.Equal(t, "yc-preference", done["service"])
		assert.Equal(t, "deleted", done["action"])
		assert.EqualValues(t, 3, done["record_count"], "preferences, email and sms")
		assert.Equal(t, http.StatusNotFound, svc.do("GET", "/internal/preferences/"+bob, nil, nil))
		assert.Equal(t, http.StatusOK, svc.do("GET", "/internal/preferences/"+alice, nil, nil), "others are kept")
	})

	t.Run("roles", func(t *testing.T) {
		visitor := as(t, srv, "shop-a", uuid.NewString(), "visitor")
		assert.Equal(t, http.StatusForbidden, visitor.do("GET", "/preferences/me", nil, nil))
		assert.Equal(t, http.StatusForbidden, aliceC.do("GET", "/admin/preferences/defaults", nil, nil))
		var p api.Preferences
		assert.Equal(t, http.StatusOK, admin.do("GET", "/preferences/me", nil, &p), "shop admins keep preferences too")
	})
}
