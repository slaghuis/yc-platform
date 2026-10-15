//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"

	api "github.com/slaghuis/yc-platform/services/yc-payment/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/app"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe/stripetest"
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

// service is a platform service calling through the gateway's internal
// listener.
func service(t *testing.T, srv *httptest.Server, tenant, name string) client {
	c := as(t, srv, tenant, name, "service")
	c.hdr[authctx.HeaderCallerService] = name
	return c
}

func (c client) raw(method, path string, body []byte, out any, header ...string) int {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, bytes.NewReader(body))
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if out != nil && len(b) > 0 && resp.StatusCode < 300 {
		require.NoError(c.t, json.Unmarshal(b, out), string(b))
	}
	return resp.StatusCode
}

func (c client) do(method, path string, body, out any, header ...string) int {
	c.t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		require.NoError(c.t, err)
	}
	return c.raw(method, path, b, out, header...)
}

func TestPayments(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-payment")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	fake := stripetest.NewServer(t)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	dbURL := testkit.Postgres(t)
	t.Setenv("YC_PAYMENT_DB_URL", dbURL)
	t.Setenv("NATS_URL", testkit.NATS(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_PAYMENT_CIPHER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("YC_PAYMENT_STRIPE_API_BASE", fake.URL)
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-payment", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	outbox := func(tenant, eventType string) []map[string]any {
		t.Helper()
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
	eventsFor := func(eventType, txID string) int {
		n := 0
		for _, e := range outbox("shop-a", eventType) {
			if e["transaction_id"] == txID {
				n++
			}
		}
		return n
	}

	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	checkout := service(t, srv, "shop-a", "yc-checkout")
	orders := service(t, srv, "shop-a", "yc-orders")
	// stripeHook is Stripe posting to shop-a's host: the gateway forwards
	// callbacks with the tenant and no actor.
	stripeHook := client{t: t, srv: srv, hdr: map[string]string{authctx.HeaderTenantID: "shop-a"}}
	const secret = "whsec_test_shop_a"

	authorize := func(session, card, key string, amount int64) (int, api.Transaction) {
		t.Helper()
		var tx api.Transaction
		code := checkout.do("POST", "/internal/authorize", api.AuthorizeInput{
			SessionID: session, Amount: api.Money{AmountMinor: amount, Currency: "ZAR"}, PaymentToken: card,
		}, &tx, "Idempotency-Key", key)
		return code, tx
	}
	capture := func(id uuid.UUID, order *uuid.UUID) (int, api.Transaction) {
		t.Helper()
		var tx api.Transaction
		code := checkout.do("POST", "/internal/capture", api.CaptureInput{TransactionID: id, OrderID: order}, &tx)
		return code, tx
	}
	webhook := func(eventType, object string) int {
		t.Helper()
		body, sig := fake.Webhook(secret, eventType, object)
		return stripeHook.raw("POST", "/webhooks/stripe", body, nil, stripe.SignatureHeader, sig)
	}

	t.Run("a shop without credentials cannot take payments", func(t *testing.T) {
		code, _ := authorize(uuid.NewString(), stripetest.CardVisa, "s0:authorize", 1000)
		assert.Equal(t, http.StatusPreconditionFailed, code)
		var caps api.Capabilities
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/gateway-capabilities/stripe", nil, &caps))
		assert.True(t, caps.SupportsSeparateAuthCapture)
		assert.False(t, caps.Configured)
	})

	t.Run("shop admins store credentials encrypted and see them masked", func(t *testing.T) {
		customer := as(t, srv, "shop-a", uuid.NewString(), "customer")
		assert.Equal(t, http.StatusForbidden, customer.do("GET", "/admin/gateway-config", nil, nil))

		var c api.GatewayConfig
		sk, ws, pk := "sk_test_51abcdef4242", secret, "pk_test_51abc"
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/gateway-config/stripe", api.GatewayConfigInput{
			Enabled: true, APIKey: &sk, WebhookSecret: &ws, PublicKey: &pk,
		}, &c))
		assert.Equal(t, "sk_test_…4242", c.APIKeyHint)
		assert.True(t, c.WebhookSecretSet)

		var list api.GatewayConfigList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/gateway-config", nil, &list))
		require.Len(t, list.Items, 1)
		raw, _ := json.Marshal(list)
		assert.NotContains(t, string(raw), sk)
		assert.NotContains(t, string(raw), ws)

		var plain int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM tenant_gateway_config
			WHERE position($1::bytea IN credentials_enc) > 0`, []byte(sk)).Scan(&plain))
		assert.Zero(t, plain, "the key is not stored in clear")

		var res api.GatewayTestResult
		require.Equal(t, http.StatusOK, admin.do("POST", "/admin/gateway-config/stripe/test", nil, &res))
		assert.True(t, res.Ok)

		// Rotating the key keeps the webhook secret.
		bad := "sk_live_nope0000"
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/gateway-config/stripe",
			api.GatewayConfigInput{Enabled: true, APIKey: &bad}, &c))
		assert.True(t, c.WebhookSecretSet)
		require.Equal(t, http.StatusOK, admin.do("POST", "/admin/gateway-config/stripe/test", nil, &res))
		assert.False(t, res.Ok)
		require.NotNil(t, res.Message)
		assert.Contains(t, *res.Message, "api_key_invalid")
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/gateway-config/stripe",
			api.GatewayConfigInput{Enabled: true, APIKey: &sk, PublicKey: &pk}, &c))

		var caps api.Capabilities
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/gateway-capabilities/stripe", nil, &caps))
		assert.True(t, caps.Configured)
		assert.Equal(t, pk, *caps.PublicKey)
	})

	var paid api.Transaction
	t.Run("authorize, capture and refund", func(t *testing.T) {
		session := uuid.NewString()
		code, tx := authorize(session, stripetest.CardVisa, session+":authorize", 12345)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("authorized"), tx.Status)
		require.NotNil(t, tx.GatewayReference)
		pi, ok := fake.Intent(*tx.GatewayReference)
		require.True(t, ok)
		assert.Equal(t, "manual", pi.CaptureMethod)
		assert.Equal(t, tx.TransactionID.String(), pi.Metadata["transaction_id"])
		assert.Equal(t, 1, eventsFor(events.TypePaymentIntentCreatedV1, tx.TransactionID.String()))
		assert.Equal(t, 1, eventsFor(events.TypePaymentAuthorizedV1, tx.TransactionID.String()))

		creates := fake.Calls("POST /v1/payment_intents")
		code, again := authorize(session, stripetest.CardVisa, session+":authorize", 12345)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, tx.TransactionID, again.TransactionID, "a repeated key returns the transaction")
		assert.Equal(t, creates, fake.Calls("POST /v1/payment_intents"), "without calling Stripe again")
		code, _ = authorize(session, stripetest.CardVisa, session+":authorize", 999)
		assert.Equal(t, http.StatusConflict, code, "the same key for a different payment")

		order := uuid.New()
		code, tx = capture(tx.TransactionID, &order)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("captured"), tx.Status)
		assert.EqualValues(t, 12345, tx.CapturedAmountMinor)
		code, _ = capture(tx.TransactionID, &order)
		require.Equal(t, http.StatusOK, code, "capturing again is a no-op")
		captured := outbox("shop-a", events.TypePaymentCapturedV1)
		require.Len(t, captured, 1)
		assert.Equal(t, order.String(), captured[0]["order_id"])

		var void api.VoidResult
		assert.Equal(t, http.StatusConflict, checkout.do("POST", "/internal/void",
			api.VoidInput{TransactionID: &tx.TransactionID}, &void), "a captured payment is refunded, not voided")

		var r api.RefundResult
		part := int64(2345)
		reason := "damaged"
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID, AmountMinor: &part, Reason: &reason,
		}, &r, "Idempotency-Key", order.String()+":refund:1"))
		assert.Equal(t, api.RefundResultStatus("completed"), r.Status)
		require.NotNil(t, r.Refund)
		require.NotNil(t, r.Refund.OrderID)
		assert.Equal(t, order, *r.Refund.OrderID, "the order comes from the transaction")
		var r2 api.RefundResult
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID, AmountMinor: &part,
		}, &r2, "Idempotency-Key", order.String()+":refund:1"))
		assert.Equal(t, r.Refund.RefundID, r2.Refund.RefundID, "a repeated key returns the refund")

		too := int64(20000)
		assert.Equal(t, http.StatusBadRequest, orders.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID, AmountMinor: &too,
		}, nil, "Idempotency-Key", order.String()+":refund:2"))
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID,
		}, &r, "Idempotency-Key", order.String()+":refund:3"))
		assert.EqualValues(t, 10000, r.Refund.AmountMinor, "the rest")
		assert.Equal(t, http.StatusConflict, orders.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID,
		}, nil, "Idempotency-Key", order.String()+":refund:4"))

		var d api.TransactionDetail
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/transactions/"+tx.TransactionID.String(), nil, &d))
		assert.Equal(t, api.TransactionStatus("refunded"), d.Status)
		assert.EqualValues(t, 12345, d.RefundedAmountMinor)
		assert.Len(t, d.Refunds, 2)
		assert.Len(t, outbox("shop-a", events.TypePaymentRefundInitiatedV1), 2)
		assert.Len(t, outbox("shop-a", events.TypePaymentRefundCompletedV1), 2)
		paid = tx
	})

	t.Run("a declined card is a failed transaction", func(t *testing.T) {
		session := uuid.NewString()
		code, tx := authorize(session, stripetest.CardDeclined, session+":authorize", 5000)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("failed"), tx.Status)
		require.NotNil(t, tx.FailureReason)
		assert.Equal(t, "generic_decline", *tx.FailureReason)
		failed := outbox("shop-a", events.TypePaymentAuthorizationFailedV1)
		require.NotEmpty(t, failed)
		assert.Equal(t, "generic_decline", failed[len(failed)-1]["reason"])

		code, _ = capture(tx.TransactionID, nil)
		assert.Equal(t, http.StatusConflict, code)
		var void api.VoidResult
		authKey := session + ":authorize"
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/void",
			api.VoidInput{AuthorizationKey: &authKey}, &void))
		assert.Equal(t, api.VoidResultStatus("failed"), void.Status, "nothing to void")
	})

	t.Run("3-D Secure is pending until the webhook", func(t *testing.T) {
		session := uuid.NewString()
		code, tx := authorize(session, stripetest.Card3DS, session+":authorize", 7000)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("pending"), tx.Status)
		require.NotNil(t, tx.NextAction)
		assert.Equal(t, "use_stripe_sdk", (*tx.NextAction)["type"])
		code, tx = authorize(session, stripetest.Card3DS, session+":authorize", 7000)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("pending"), tx.Status, "still waiting on the customer")

		fake.CompleteAction(*tx.GatewayReference, true)
		body, sig := fake.Webhook(secret, "payment_intent.amount_capturable_updated", *tx.GatewayReference)
		require.Equal(t, http.StatusOK, stripeHook.raw("POST", "/webhooks/stripe", body, nil, stripe.SignatureHeader, sig))
		require.Equal(t, http.StatusOK, stripeHook.raw("POST", "/webhooks/stripe", body, nil, stripe.SignatureHeader, sig),
			"a redelivery is acknowledged")
		var d api.TransactionDetail
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/transactions/"+tx.TransactionID.String(), nil, &d))
		assert.Equal(t, api.TransactionStatus("authorized"), d.Status)
		assert.Nil(t, d.NextAction)
		assert.Equal(t, 1, eventsFor(events.TypePaymentAuthorizedV1, tx.TransactionID.String()))

		var void api.VoidResult
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/void",
			api.VoidInput{TransactionID: &tx.TransactionID}, &void))
		assert.Equal(t, api.VoidResultStatus("voided"), void.Status)
		assert.Equal(t, 1, eventsFor(events.TypePaymentVoidedV1, tx.TransactionID.String()))
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/void",
			api.VoidInput{TransactionID: &tx.TransactionID}, &void))
		assert.Equal(t, 1, eventsFor(events.TypePaymentVoidedV1, tx.TransactionID.String()), "voiding again is a no-op")
	})

	t.Run("a failed 3-D Secure challenge fails by repeated call", func(t *testing.T) {
		session := uuid.NewString()
		_, tx := authorize(session, stripetest.Card3DS, session+":authorize", 7000)
		fake.CompleteAction(*tx.GatewayReference, false)
		code, tx := authorize(session, stripetest.Card3DS, session+":authorize", 7000)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("failed"), tx.Status)
		assert.Equal(t, "payment_intent_authentication_failure", *tx.FailureReason)
	})

	t.Run("a gateway outage is retried with the same key", func(t *testing.T) {
		session := uuid.NewString()
		fake.FailNext(1)
		code, _ := authorize(session, stripetest.CardVisa, session+":authorize", 3000)
		require.Equal(t, http.StatusServiceUnavailable, code)
		code, tx := authorize(session, stripetest.CardVisa, session+":authorize", 3000)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("authorized"), tx.Status)
		assert.Equal(t, 1, eventsFor(events.TypePaymentIntentCreatedV1, tx.TransactionID.String()))
		assert.Equal(t, 1, eventsFor(events.TypePaymentAuthorizedV1, tx.TransactionID.String()))
	})

	t.Run("nothing to undo is success", func(t *testing.T) {
		var void api.VoidResult
		unknown := "never:authorize"
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/void", api.VoidInput{AuthorizationKey: &unknown}, &void))
		assert.Equal(t, api.VoidResultStatus("none"), void.Status)
		var r api.RefundResult
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/refund", api.RefundInput{TransactionID: uuid.New()},
			&r, "Idempotency-Key", "never:refund"))
		assert.Equal(t, api.RefundResultStatus("none"), r.Status)
	})

	t.Run("a pending refund completes by webhook", func(t *testing.T) {
		session := uuid.NewString()
		_, tx := authorize(session, stripetest.CardVisa, session+":authorize", 4000)
		code, _ := capture(tx.TransactionID, nil)
		require.Equal(t, http.StatusOK, code)
		fake.RefundsPending = true
		t.Cleanup(func() { fake.RefundsPending = false })
		var r api.RefundResult
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/refund", api.RefundInput{TransactionID: tx.TransactionID},
			&r, "Idempotency-Key", session+":refund"))
		assert.Equal(t, api.RefundResultStatus("pending"), r.Status)

		fake.SettleRefund(*r.Refund.GatewayReference, true)
		require.Equal(t, http.StatusOK, webhook("refund.updated", *r.Refund.GatewayReference))
		var d api.TransactionDetail
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/transactions/"+tx.TransactionID.String(), nil, &d))
		assert.Equal(t, api.TransactionStatus("refunded"), d.Status)
		require.Len(t, d.Refunds, 1)
		assert.Equal(t, api.RefundStatus("completed"), d.Refunds[0].Status)
	})

	t.Run("webhooks with a bad signature are rejected", func(t *testing.T) {
		body, _ := fake.Webhook(secret, "payment_intent.succeeded", *paid.GatewayReference)
		_, forged := fake.Webhook("whsec_attacker", "payment_intent.succeeded", *paid.GatewayReference)
		assert.Equal(t, http.StatusUnauthorized, stripeHook.raw("POST", "/webhooks/stripe", body, nil))
		assert.Equal(t, http.StatusUnauthorized,
			stripeHook.raw("POST", "/webhooks/stripe", body, nil, stripe.SignatureHeader, forged))
		assert.Len(t, outbox("shop-a", events.TypePaymentWebhookSignatureFailedV1), 2)
		other := client{t: t, srv: srv, hdr: map[string]string{authctx.HeaderTenantID: "shop-b"}}
		_, sig := fake.Webhook(secret, "payment_intent.succeeded", *paid.GatewayReference)
		assert.Equal(t, http.StatusPreconditionFailed,
			other.raw("POST", "/webhooks/stripe", body, nil, stripe.SignatureHeader, sig), "shop-b has no secret")
	})

	t.Run("internal routes need a service caller; shops are isolated", func(t *testing.T) {
		customer := as(t, srv, "shop-a", uuid.NewString(), "customer")
		assert.Equal(t, http.StatusUnauthorized, customer.do("POST", "/internal/capture",
			api.CaptureInput{TransactionID: paid.TransactionID}, nil))
		shopB := service(t, srv, "shop-b", "yc-checkout")
		assert.Equal(t, http.StatusNotFound, shopB.do("GET", "/internal/transactions/"+paid.TransactionID.String(), nil, nil))
		code, _ := authorize(uuid.NewString(), stripetest.CardVisa, "", 100)
		assert.Equal(t, http.StatusBadRequest, code, "Idempotency-Key is required")
	})

	t.Run("admins page through transactions", func(t *testing.T) {
		var page api.TransactionList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/transactions?page_size=3", nil, &page))
		require.Len(t, page.Items, 3)
		require.NotEmpty(t, page.Page.NextCursor)
		seen := map[uuid.UUID]bool{}
		for _, tx := range page.Items {
			seen[tx.TransactionID] = true
		}
		var page2 api.TransactionList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/transactions?page_size=3&cursor="+page.Page.NextCursor, nil, &page2))
		require.NotEmpty(t, page2.Items)
		for _, tx := range page2.Items {
			assert.False(t, seen[tx.TransactionID], "pages do not overlap")
		}
		var failed api.TransactionList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/transactions?status=failed", nil, &failed))
		require.NotEmpty(t, failed.Items)
		for _, tx := range failed.Items {
			assert.Equal(t, api.TransactionStatus("failed"), tx.Status)
		}
	})
}
