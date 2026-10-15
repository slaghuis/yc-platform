//go:build integration

package app_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"

	api "github.com/slaghuis/yc-platform/services/yc-payment/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/app"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe/stripetest"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/yoco/yocotest"
)

// TestYoco covers Phase 5 in yc-payment: per-currency routing, Yoco's
// combined charge on a hosted page, its webhooks and refunds.
func TestYoco(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-payment")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	stripeFake := stripetest.NewServer(t)
	fake := yocotest.NewServer(t)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	dbURL := testkit.Postgres(t)
	t.Setenv("YC_PAYMENT_DB_URL", dbURL)
	t.Setenv("NATS_URL", testkit.NATS(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_PAYMENT_CIPHER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("YC_PAYMENT_STRIPE_API_BASE", stripeFake.URL)
	t.Setenv("YC_PAYMENT_YOCO_API_BASE", fake.URL)
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-payment", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	// shopHost stands in for yc-gateway on shop-y's host: it forwards
	// callbacks with the tenant and no actor.
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	shopHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(authctx.HeaderTenantID, "shop-y")
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(shopHost.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	eventsFor := func(eventType, txID string) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE tenant_id = 'shop-y'
			AND envelope->>'event_type' = $1 AND envelope->'payload'->>'transaction_id' = $2`, eventType, txID).Scan(&n))
		return n
	}

	admin := as(t, srv, "shop-y", uuid.NewString(), "shop_admin")
	checkout := service(t, srv, "shop-y", "yc-checkout")
	const yocoKey = "sk_test_yoco_shop_y"
	hook := fake.Register(yocoKey, "yc", shopHost.URL+"/webhooks/yoco")

	route := func(query string) (int, api.Capabilities) {
		t.Helper()
		var c api.Capabilities
		return checkout.do("GET", "/internal/gateway-route?"+query, nil, &c), c
	}
	chargeIn := func(session string) api.ChargeInput {
		ret, order := "https://shop-y.test/checkout/return", uuid.New()
		return api.ChargeInput{
			SessionID: session, Gateway: "yoco", Amount: api.Money{AmountMinor: 23000, Currency: "ZAR"},
			ReturnURL: &ret, OrderID: &order,
		}
	}
	charge := func(in api.ChargeInput, key string) (int, api.Transaction) {
		t.Helper()
		var tx api.Transaction
		return checkout.do("POST", "/internal/charge", in, &tx, "Idempotency-Key", key), tx
	}
	get := func(id uuid.UUID) api.TransactionDetail {
		t.Helper()
		var tx api.TransactionDetail
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/transactions/"+id.String(), nil, &tx))
		return tx
	}

	t.Run("routing picks the gateway by priority and currency", func(t *testing.T) {
		code, _ := route("currency=ZAR")
		assert.Equal(t, http.StatusPreconditionFailed, code, "no gateway set up")

		sk, stripePriority := "sk_test_stripe_shop_y", 20
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/gateway-config/stripe",
			api.GatewayConfigInput{Enabled: true, APIKey: &sk, Priority: &stripePriority}, nil))
		yk, ws, yocoPriority := yocoKey, hook.Secret, 10
		usd := []string{"USD"}
		code = admin.do("PUT", "/admin/gateway-config/yoco",
			api.GatewayConfigInput{Enabled: true, APIKey: &yk, WebhookSecret: &ws, Priority: &yocoPriority, Currencies: &usd}, nil)
		assert.Equal(t, http.StatusBadRequest, code, "Yoco takes only ZAR")
		var c api.GatewayConfig
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/gateway-config/yoco",
			api.GatewayConfigInput{Enabled: true, APIKey: &yk, WebhookSecret: &ws, Priority: &yocoPriority}, &c))
		assert.Equal(t, 10, c.Priority)
		assert.Empty(t, c.Currencies)

		var res api.GatewayTestResult
		require.Equal(t, http.StatusOK, admin.do("POST", "/admin/gateway-config/yoco/test", nil, &res))
		assert.True(t, res.Ok)

		code, caps := route("currency=ZAR")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "yoco", caps.Gateway, "Yoco has priority for ZAR")
		assert.False(t, caps.SupportsSeparateAuthCapture, "Variant B")
		assert.True(t, caps.HostedPayment)
		require.NotNil(t, caps.SupportedCurrencies)
		assert.Equal(t, []string{"ZAR"}, *caps.SupportedCurrencies)

		code, caps = route("currency=USD")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "stripe", caps.Gateway, "Yoco does not take USD")
		assert.True(t, caps.SupportsSeparateAuthCapture, "Variant A")
		assert.False(t, caps.HostedPayment)

		code, caps = route("currency=ZAR&gateway=stripe")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "stripe", caps.Gateway, "the customer may choose another gateway that qualifies")
		code, _ = route("currency=USD&gateway=yoco")
		assert.Equal(t, http.StatusPreconditionFailed, code)
		code, _ = route("currency=usd")
		assert.Equal(t, http.StatusBadRequest, code)

		var list api.GatewayConfigList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/gateway-config", nil, &list))
		require.Len(t, list.Items, 2)
		assert.Equal(t, "yoco", list.Items[0].Gateway, "listed in priority order")
	})

	t.Run("Yoco cannot authorize, and a charge needs a return URL and no token", func(t *testing.T) {
		code := checkout.do("POST", "/internal/authorize", api.AuthorizeInput{
			SessionID: uuid.NewString(), Gateway: ptr(api.AuthorizeInputGateway("yoco")),
			Amount: api.Money{AmountMinor: 1000, Currency: "ZAR"}, PaymentToken: "pm_card_visa",
		}, nil, "Idempotency-Key", uuid.NewString())
		assert.Equal(t, http.StatusBadRequest, code)

		in := chargeIn(uuid.NewString())
		in.ReturnURL = nil
		code, _ = charge(in, uuid.NewString())
		assert.Equal(t, http.StatusBadRequest, code)
		in = chargeIn(uuid.NewString())
		in.PaymentToken = ptr("pm_card_visa")
		code, _ = charge(in, uuid.NewString())
		assert.Equal(t, http.StatusBadRequest, code)
		in = chargeIn(uuid.NewString())
		in.Amount.Currency = "USD"
		code, _ = charge(in, uuid.NewString())
		assert.Equal(t, http.StatusBadRequest, code)
	})

	t.Run("a hosted charge waits for the webhook; a replay changes nothing", func(t *testing.T) {
		session := uuid.NewString()
		in := chargeIn(session)
		code, tx := charge(in, session+":charge")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("pending"), tx.Status)
		assert.Equal(t, api.TransactionKind("charge"), *tx.Kind)
		require.NotNil(t, tx.NextAction)
		assert.Equal(t, "redirect_to_url", (*tx.NextAction)["type"])
		require.NotNil(t, tx.GatewayReference)
		require.NotNil(t, tx.OrderID)
		assert.Equal(t, *in.OrderID, *tx.OrderID)
		checkoutID := *tx.GatewayReference

		code, again := charge(in, session+":charge")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, tx.TransactionID, again.TransactionID)
		assert.Equal(t, api.TransactionStatus("pending"), again.Status, "Yoco cannot be asked; the stored state is the answer")
		assert.Equal(t, 1, fake.Calls("POST /api/checkouts"), "the repeat did not create another checkout")

		ev, deliveries, err := fake.Pay(ctx, checkoutID, true)
		require.NoError(t, err)
		require.Len(t, deliveries, 1)
		require.Equal(t, http.StatusOK, deliveries[0].Status, deliveries[0].Error)
		paid := get(tx.TransactionID)
		assert.Equal(t, api.TransactionStatus("captured"), paid.Status)
		assert.EqualValues(t, 23000, paid.CapturedAmountMinor)
		assert.Nil(t, paid.NextAction)
		assert.Equal(t, 1, eventsFor(events.TypePaymentChargedV1, tx.TransactionID.String()))
		assert.Zero(t, eventsFor(events.TypePaymentCapturedV1, tx.TransactionID.String()), "a charge is not a capture")

		redelivered, ok := fake.Redeliver(ctx, ev.ID)
		require.True(t, ok)
		require.Equal(t, http.StatusOK, redelivered[0].Status)
		assert.Equal(t, 1, eventsFor(events.TypePaymentChargedV1, tx.TransactionID.String()), "a replayed webhook is a no-op")
		var seen int
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-y", func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE gateway = 'yoco'
				AND gateway_event_id = $1`, ev.ID).Scan(&seen)
		}))
		assert.Equal(t, 1, seen)

		code, again = charge(in, session+":charge")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.TransactionStatus("captured"), again.Status, "repeating the call learns the outcome")
	})

	t.Run("a declined payment fails the charge; paying after all still counts", func(t *testing.T) {
		session := uuid.NewString()
		_, tx := charge(chargeIn(session), session+":charge")
		_, _, err := fake.Pay(ctx, *tx.GatewayReference, false)
		require.NoError(t, err)
		failed := get(tx.TransactionID)
		assert.Equal(t, api.TransactionStatus("failed"), failed.Status)
		assert.Equal(t, "payment_failed", *failed.FailureReason)
		assert.Equal(t, 1, eventsFor(events.TypePaymentChargeFailedV1, tx.TransactionID.String()))

		_, _, err = fake.Pay(ctx, *tx.GatewayReference, true)
		require.NoError(t, err)
		assert.Equal(t, api.TransactionStatus("captured"), get(tx.TransactionID).Status,
			"the customer tried another card on Yoco's page")
		assert.Equal(t, 1, eventsFor(events.TypePaymentChargedV1, tx.TransactionID.String()))
	})

	t.Run("an unpaid Yoco checkout cannot be voided; a late payment is refunded", func(t *testing.T) {
		session := uuid.NewString()
		_, tx := charge(chargeIn(session), session+":charge")
		var void api.VoidResult
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/void",
			api.VoidInput{AuthorizationKey: ptr(session + ":charge")}, &void))
		assert.Equal(t, api.VoidResultStatus("pending"), void.Status, "it stays payable")

		_, _, err := fake.Pay(ctx, *tx.GatewayReference, true)
		require.NoError(t, err)
		code := checkout.do("POST", "/internal/void", api.VoidInput{TransactionID: &tx.TransactionID}, nil)
		assert.Equal(t, http.StatusConflict, code, "paid: refund it")

		fake.RefundsPending = true
		var r api.RefundResult
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/refund", api.RefundInput{
			TransactionID: tx.TransactionID, Reason: ptr("late charge"),
		}, &r, "Idempotency-Key", session+":charge:refund"))
		assert.Equal(t, api.RefundResultStatus("pending"), r.Status)
		require.NotNil(t, r.Refund.GatewayReference)
		require.Len(t, fake.SettleRefund(ctx, *r.Refund.GatewayReference, true), 1)

		refunded := get(tx.TransactionID)
		assert.Equal(t, api.TransactionStatus("refunded"), refunded.Status)
		require.Len(t, refunded.Refunds, 1)
		assert.Equal(t, api.RefundStatus("completed"), refunded.Refunds[0].Status)
		assert.Equal(t, 1, eventsFor(events.TypePaymentRefundCompletedV1, tx.TransactionID.String()))
	})

	t.Run("a bad signature is refused and announced", func(t *testing.T) {
		session := uuid.NewString()
		_, tx := charge(chargeIn(session), session+":charge")
		ev, _, err := fake.Pay(ctx, *tx.GatewayReference, true)
		require.NoError(t, err)
		forged := yocotest.Headers("whsec_"+base64.StdEncoding.EncodeToString([]byte("not the shop's secret")), ev)
		hdr := make([]string, 0, 2*len(forged))
		for k := range forged {
			hdr = append(hdr, k, forged.Get(k))
		}
		hostHook := client{t: t, srv: srv, hdr: map[string]string{authctx.HeaderTenantID: "shop-y"}}
		assert.Equal(t, http.StatusUnauthorized, hostHook.raw("POST", "/webhooks/yoco", ev.Body, nil, hdr...))
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE tenant_id = 'shop-y'
			AND envelope->>'event_type' = $1`, events.TypePaymentWebhookSignatureFailedV1).Scan(&n))
		assert.Equal(t, 1, n)
	})
}

func ptr[T any](v T) *T { return &v }
