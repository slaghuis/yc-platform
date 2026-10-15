//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"

	api "github.com/slaghuis/yc-platform/services/yc-checkout/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/app"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
)

const shop = "shop-a"

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

func as(t *testing.T, srv *httptest.Server, actorID, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  shop,
		authctx.HeaderActorID:   actorID,
		authctx.HeaderActorType: actorType,
	}}
}

func (c client) do(method, path string, body, out any, header ...string) int {
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
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if out != nil {
		require.NoError(c.t, json.Unmarshal(b, out), string(b))
	}
	return resp.StatusCode
}

func (c client) start() api.Session {
	c.t.Helper()
	var s api.Session
	require.Equal(c.t, http.StatusCreated, c.do(http.MethodPost, "/checkout", map[string]any{}, &s))
	return s
}

func (c client) confirm(id uuid.UUID, key string, body map[string]any) (int, api.Session) {
	c.t.Helper()
	var s api.Session
	code := c.do(http.MethodPost, "/checkout/"+id.String()+"/confirm", body, &s, httpkit.HeaderIdempotencyKey, key)
	return code, s
}

func (c client) get(id uuid.UUID) api.Session {
	c.t.Helper()
	var s api.Session
	require.Equal(c.t, http.StatusOK, c.do(http.MethodGet, "/checkout/"+id.String(), nil, &s))
	return s
}

var address = map[string]any{"line1": "1 Long St", "city": "Cape Town", "country": "ZA"}

func pay(token string) map[string]any {
	return map[string]any{"delivery_address": address, "payment_gateway_token": token}
}

// filter keeps the calls to the given steps.
func filter(calls []string, steps ...string) []string {
	var out []string
	for _, c := range calls {
		if slices.Contains(steps, strings.Fields(c)[0]) {
			out = append(out, c)
		}
	}
	return out
}

func TestCheckout(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-checkout")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	gw := newFakeGateway()
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	dbURL := testkit.Postgres(t)
	t.Setenv("YC_CHECKOUT_DB_URL", dbURL)
	t.Setenv("NATS_URL", testkit.NATS(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gwSrv.URL)
	t.Setenv("YC_CHECKOUT_SERVICE_SECRET", "checkout-secret")
	t.Setenv("YC_CHECKOUT_SHIPPING_FEE_MINOR", "10000")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	cfg.Saga.SweeperInterval = time.Hour
	cfg.Saga.RetryBase, cfg.Saga.RetryMax = time.Millisecond, time.Millisecond
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-checkout", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	inShop := func(sql string, args ...any) {
		t.Helper()
		require.NoError(t, pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, shop); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, sql, args...)
			return err
		}))
	}
	outbox := func(sessionID uuid.UUID) []string {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT envelope->>'event_type' FROM outbox
			WHERE envelope->'payload'->>'session_id' = $1 ORDER BY created_at, id`, sessionID.String())
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		return out
	}

	mug := gw.sku(fakeSKU{Name: "Mug", Tracked: true, Price: 11500, Stock: 10})
	ebook := gw.sku(fakeSKU{Name: "E-book", Digital: true, Price: 23000})
	lamp := gw.sku(fakeSKU{Name: "Lamp", Tracked: true, Price: 50000, Stock: 0})
	retired := gw.sku(fakeSKU{Name: "Old", Price: 100, Inactive: true})

	customer := func(t *testing.T, lines ...fakeLine) client {
		id := uuid.NewString()
		gw.setCart(id, lines...)
		return as(t, srv, id, "customer")
	}

	t.Run("happy path: tracked and digital items", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 2}, fakeLine{ebook, 1})
		s := c.start()
		assert.Equal(t, api.PRICING, s.State)
		assert.True(t, s.RequiresShipping)
		require.Len(t, s.Items, 2)
		assert.Equal(t, int64(23000), s.Items[0].LineTotal.AmountMinor)
		assert.Equal(t, int64(46000), s.Totals.ItemsTotal.AmountMinor)
		assert.Equal(t, int64(3000+3000), s.Totals.TaxTotal.AmountMinor)
		assert.Equal(t, int64(10000), s.Totals.Shipping.AmountMinor, "flat fee, physical items")
		assert.Equal(t, int64(56000), s.Totals.Total.AmountMinor)
		assert.Equal(t, "standard", *s.SelectedShippingOptionID)
		assert.WithinDuration(t, time.Now().Add(30*time.Minute), s.ExpiresAt, time.Minute)

		// ADR-010: a physical checkout needs an address.
		var prob map[string]any
		code := c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/confirm",
			map[string]any{"payment_gateway_token": cardVisa}, &prob, httpkit.HeaderIdempotencyKey, "k1")
		require.Equal(t, http.StatusBadRequest, code)
		assert.Contains(t, prob["errors"].([]any)[0].(map[string]any)["field"], "delivery_address")

		code, s = c.confirm(s.SessionID, "k1", pay(cardVisa))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.COMPLETED, s.State)
		require.NotNil(t, s.Order)
		assert.Regexp(t, `^SHOP-A-\d{6}$`, *s.Order.OrderNumber)
		assert.Equal(t, "captured", *s.Payment.Status)
		assert.Nil(t, s.Failure)
		assert.Equal(t, int64(8), gw.stock(mug), "only the tracked SKU is held")

		assert.Equal(t, []string{
			"reservations S:reserve", "authorize S:authorize", "orders S:order", "capture S:capture",
			"orders.confirm S:confirm", "reservations.commit S:commit",
		}, gw.callsFor(s.SessionID.String()))
		o := gw.orderFor(s.SessionID.String())
		assert.Equal(t, "received", o.Status)
		assert.Equal(t, float64(56000), o.Body["total_minor"])
		assert.Equal(t, float64(10000), o.Body["shipping_minor"])
		assert.Equal(t, []string{
			"checkout.started.v1", "checkout.priced.v1", "checkout.validation_failed.v1",
			"checkout.confirmed.v1", "checkout.completed.v1",
		}, outbox(s.SessionID))

		// The same key replays the outcome; another key is a new attempt.
		code, again := c.confirm(s.SessionID, "k1", pay(cardVisa))
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, s.Order.OrderID, again.Order.OrderID)
		assert.Equal(t, http.StatusConflict, c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/confirm",
			pay(cardVisa), nil, httpkit.HeaderIdempotencyKey, "k2"))
		assert.Len(t, gw.callsFor(s.SessionID.String()), 6, "no participant called again")

		// Only the owner sees the session.
		other := as(t, srv, uuid.NewString(), "customer")
		assert.Equal(t, http.StatusNotFound, other.do(http.MethodGet, "/checkout/"+s.SessionID.String(), nil, nil))
	})

	t.Run("guest digital-only checkout skips reserving and shipping", func(t *testing.T) {
		visitor := uuid.NewString()
		gw.setCart(visitor, fakeLine{ebook, 1})
		v := as(t, srv, visitor, "visitor")
		s := v.start()
		assert.False(t, s.RequiresShipping)
		assert.Empty(t, s.ShippingOptions)
		assert.Equal(t, int64(23000), s.Totals.Total.AmountMinor)

		var prob map[string]any
		code := v.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/confirm",
			map[string]any{"payment_gateway_token": cardVisa, "guest_contact": map[string]any{"email": "nope", "phone_e164": "0831234567"}},
			&prob, httpkit.HeaderIdempotencyKey, "g1")
		require.Equal(t, http.StatusBadRequest, code)
		assert.Len(t, prob["errors"], 2)

		code, s = v.confirm(s.SessionID, "g1", map[string]any{
			"payment_gateway_token": cardVisa,
			"guest_contact":         map[string]any{"email": "guest@example.com", "phone_e164": "+27831234567"},
		})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.COMPLETED, s.State)
		assert.Nil(t, s.Reservation)
		assert.Equal(t, []string{"authorize S:authorize", "orders S:order", "capture S:capture", "orders.confirm S:confirm"},
			gw.callsFor(s.SessionID.String()))
		assert.NotNil(t, gw.orderFor(s.SessionID.String()).Body["guest_contact"])
	})

	t.Run("start refuses empty, unavailable and non-convertible carts", func(t *testing.T) {
		assert.Equal(t, http.StatusConflict, customer(t).do(http.MethodPost, "/checkout", nil, nil))
		var prob map[string]any
		assert.Equal(t, http.StatusConflict, customer(t, fakeLine{retired, 1}).do(http.MethodPost, "/checkout", nil, &prob))
		assert.Equal(t, []any{retired}, prob["unavailable_skus"])
		assert.Equal(t, http.StatusConflict, customer(t, fakeLine{mug, 1}).do(http.MethodPost, "/checkout",
			map[string]any{"cart_type": "wishlist"}, nil))
		assert.Equal(t, http.StatusForbidden, as(t, srv, "admin", "shop_admin").do(http.MethodPost, "/checkout", nil, nil))
	})

	t.Run("out of stock fails fast without payment", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1}, fakeLine{lamp, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "oos", pay(cardVisa))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "insufficient_stock", s.Failure.Reason)
		assert.Equal(t, api.RESERVING, s.Failure.State)
		require.NotNil(t, s.Failure.Detail)
		assert.Contains(t, *s.Failure.Detail, "shortages")
		assert.Equal(t, []string{"reservations S:reserve"}, gw.callsFor(s.SessionID.String()))
		assert.Equal(t, []string{
			"checkout.started.v1", "checkout.priced.v1", "checkout.confirmed.v1",
			"checkout.compensating.v1", "checkout.failed.v1",
		}, outbox(s.SessionID))
	})

	t.Run("declined card releases the stock", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		code, s := c.confirm(s.SessionID, "dec", pay(cardDeclined))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		assert.Equal(t, "payment_declined", s.Failure.Reason)
		assert.Equal(t, "card_declined", (*s.Failure.Detail)["decline_reason"])
		assert.Equal(t, before, gw.stock(mug))
		assert.Nil(t, gw.orderFor(s.SessionID.String()), "no order")
		assert.Equal(t, []string{
			"reservations S:reserve", "authorize S:authorize", "void S:authorize:undo",
			"reservations.release S:reserve:undo",
		}, gw.callsFor(s.SessionID.String()))
	})

	t.Run("refused capture abandons the order and voids", func(t *testing.T) {
		gw.set(func() { gw.declineCapture = true })
		defer gw.set(func() { gw.declineCapture = false })
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		code, s := c.confirm(s.SessionID, "cap", pay(cardVisa))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		assert.Equal(t, "capture_failed", s.Failure.Reason)
		assert.Equal(t, api.CAPTURING, s.Failure.State)
		assert.Equal(t, "abandoned", gw.orderFor(s.SessionID.String()).Status)
		assert.Equal(t, before, gw.stock(mug))

		var detail api.AdminSession
		admin := as(t, srv, "admin", "shop_admin")
		require.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/admin/sessions/"+s.SessionID.String(), nil, &detail))
		steps := make([]string, len(detail.CompensationLog))
		for i, e := range detail.CompensationLog {
			steps[i] = string(e.Step)
		}
		assert.Equal(t, []string{domain.UndoPayment, domain.UndoOrder, domain.UndoReservation}, steps)
		assert.NotEmpty(t, *detail.History)
	})

	t.Run("order creation outage compensates, finding the order first", func(t *testing.T) {
		gw.failNext("orders", 3)
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "ord", pay(cardVisa))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		assert.Equal(t, "step_failed", s.Failure.Reason)
		assert.Equal(t, api.ORDERCREATE, s.Failure.State)
		assert.Equal(t, "voided", gw.txFor(s.SessionID.String()).Status)
		assert.Equal(t, "abandoned", gw.orderFor(s.SessionID.String()).Status, "the repeated create finds the order to abandon")
	})

	t.Run("3-D Secure: 202 with next_action, then the same key completes", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "3ds", pay(card3DS))
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, api.AUTHORIZING, s.State)
		require.NotNil(t, s.Payment.NextAction)
		assert.Equal(t, "use_stripe_sdk", (*s.Payment.NextAction)["type"])

		gw.set(func() { gw.actions[s.SessionID.String()] = true })
		code, s = c.confirm(s.SessionID, "3ds", pay(card3DS))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.COMPLETED, s.State)
	})

	t.Run("3-D Secure settled by the payment event", func(t *testing.T) {
		c := customer(t, fakeLine{ebook, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "3ds-evt", pay(card3DS))
		require.Equal(t, http.StatusAccepted, code)
		gw.set(func() { gw.actions[s.SessionID.String()] = false })
		require.NoError(t, a.Service.PaymentSettled(ctx, shop, s.SessionID.String(), domain.StateAuthorizing))
		s = c.get(s.SessionID)
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "payment_declined", s.Failure.Reason)
	})

	t.Run("cancel while 3-D Secure is pending voids and releases", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		_, s = c.confirm(s.SessionID, "cxl", pay(card3DS))
		require.Equal(t, api.AUTHORIZING, s.State)
		require.Equal(t, http.StatusOK, c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/cancel", nil, &s))
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "cancelled", s.Failure.Reason)
		assert.Equal(t, "voided", gw.txFor(s.SessionID.String()).Status)
		assert.Equal(t, before, gw.stock(mug))

		quote := customer(t, fakeLine{ebook, 1})
		q := quote.start()
		require.Equal(t, http.StatusOK, quote.do(http.MethodPost, "/checkout/"+q.SessionID.String()+"/cancel", nil, &q))
		assert.Equal(t, api.FAILED, q.State)
	})

	t.Run("capture outage is retried, never compensated, and resumes after a crash", func(t *testing.T) {
		gw.failNext("capture", 3)
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "resume", pay(cardVisa))
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, api.CAPTURING, s.State)

		// A replica died holding the lease; it lapses and the sweeper resumes.
		inShop(`UPDATE checkout_sessions SET lease_owner = 'dead/1/x', lease_until = now() - interval '1 second',
			next_attempt_at = NULL WHERE id = $1`, s.SessionID)
		_, err := a.Service.Sweep(ctx)
		require.NoError(t, err)
		s = c.get(s.SessionID)
		assert.Equal(t, api.COMPLETED, s.State)
		assert.Equal(t, "received", gw.orderFor(s.SessionID.String()).Status)
	})

	t.Run("a capture whose reply was lost confirms instead of compensating", func(t *testing.T) {
		gw.set(func() { gw.loseCapture = true })
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		code, s := c.confirm(s.SessionID, "lost", pay(cardVisa))
		require.Equal(t, http.StatusAccepted, code)
		require.Equal(t, api.CAPTURING, s.State)
		gw.set(func() { gw.loseCapture = false })

		// The session expires meanwhile: compensation finds the payment captured.
		inShop(`UPDATE checkout_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.SessionID)
		_, err := a.Service.Expire(ctx, shop, s.SessionID.String())
		require.NoError(t, err)
		s = c.get(s.SessionID)
		assert.Equal(t, api.COMPLETED, s.State)
		assert.Equal(t, "received", gw.orderFor(s.SessionID.String()).Status)
	})

	t.Run("session expiry compensates and publishes expired", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		_, s = c.confirm(s.SessionID, "exp", pay(card3DS))
		require.Equal(t, api.AUTHORIZING, s.State)
		quote := c.start()

		inShop(`UPDATE checkout_sessions SET expires_at = now() - interval '1 second' WHERE id IN ($1, $2)`,
			s.SessionID, quote.SessionID)
		_, err := a.Service.Sweep(ctx)
		require.NoError(t, err)
		s = c.get(s.SessionID)
		assert.Equal(t, api.EXPIRED, s.State)
		assert.Equal(t, "session_expired", s.Failure.Reason)
		assert.Equal(t, "voided", gw.txFor(s.SessionID.String()).Status)
		assert.Equal(t, before, gw.stock(mug))
		assert.Contains(t, outbox(s.SessionID), "checkout.expired.v1")
		assert.Equal(t, api.EXPIRED, c.get(quote.SessionID).State)

		assert.Equal(t, http.StatusGone, c.do(http.MethodPost, "/checkout/"+quote.SessionID.String()+"/shipping",
			map[string]any{"shipping_option_id": "standard"}, nil))
	})

	t.Run("reservation expiry fails a session waiting on payment", func(t *testing.T) {
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		_, s = c.confirm(s.SessionID, "resexp", pay(card3DS))
		require.NotNil(t, s.Reservation)
		require.NoError(t, a.Service.ReservationExpired(ctx, shop, s.SessionID.String(), s.Reservation.ReservationID.String()))
		s = c.get(s.SessionID)
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "reservation_expired", s.Failure.Reason)
		assert.Equal(t, "voided", gw.txFor(s.SessionID.String()).Status)
	})

	// Variant B: the shop has Yoco first for ZAR, then Stripe.
	yoco := func(t *testing.T) {
		gw.set(func() { gw.routes = []fakeRoute{yocoRoute, stripeRoute} })
		t.Cleanup(func() { gw.set(func() { gw.routes = []fakeRoute{stripeRoute} }) })
	}
	hosted := map[string]any{"delivery_address": address, "return_url": "https://shop.example/return"}
	admin := as(t, srv, "admin", "shop_admin")
	compensation := func(t *testing.T, id uuid.UUID) map[string]string {
		t.Helper()
		var s api.AdminSession
		require.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/admin/sessions/"+id.String(), nil, &s))
		out := map[string]string{}
		for _, e := range s.CompensationLog {
			out[string(e.Step)] = e.Result
		}
		return out
	}

	t.Run("Variant B: a hosted charge waits for the webhook, then completes", func(t *testing.T) {
		yoco(t)
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		require.NotNil(t, s.Payment)
		assert.Equal(t, "yoco", s.Payment.Gateway)
		assert.Equal(t, api.Charge, s.Payment.Flow)
		assert.True(t, s.Payment.Hosted)

		var problem map[string]any
		require.Equal(t, http.StatusBadRequest, c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/confirm",
			pay(cardVisa), &problem, httpkit.HeaderIdempotencyKey, "b-bad"), "hosted: no token, and a return URL")
		assert.Contains(t, fmt.Sprint(problem["errors"]), "payment_gateway_token")
		assert.Contains(t, fmt.Sprint(problem["errors"]), "return_url")

		code, s := c.confirm(s.SessionID, "b", hosted)
		require.Equal(t, http.StatusAccepted, code)
		assert.Equal(t, api.CHARGING, s.State)
		require.NotNil(t, s.Payment.NextAction)
		assert.Equal(t, "redirect_to_url", (*s.Payment.NextAction)["type"])
		require.NotNil(t, s.Order, "the order comes before the charge")
		sid := s.SessionID.String()
		assert.Equal(t, []string{"reservations S:reserve", "orders S:order", "charge S:charge"}, gw.callsFor(sid))
		o := gw.orderFor(sid)
		assert.Equal(t, "pending_payment", o.Status)
		assert.Equal(t, map[string]any{"gateway": "yoco"}, o.Body["payment"])
		tx := gw.txFor(sid)
		assert.Equal(t, s.Order.OrderID.String(), tx.OrderID)

		require.NoError(t, a.Service.Charged(ctx, shop, sid, gw.settle(sid, true)))
		s = c.get(s.SessionID)
		assert.Equal(t, api.COMPLETED, s.State)
		assert.Equal(t, "received", gw.orderFor(sid).Status)
		assert.Equal(t, before-1, gw.stock(mug), "committed")
		assert.Contains(t, outbox(s.SessionID), "checkout.completed.v1")

		require.NoError(t, a.Service.Charged(ctx, shop, sid, tx.ID), "a redelivered event changes nothing")
		assert.Equal(t, "captured", gw.txFor(sid).Status)
		assert.Empty(t, compensation(t, s.SessionID))
	})

	t.Run("Variant B: the customer may switch to another gateway at confirm", func(t *testing.T) {
		yoco(t)
		c := customer(t, fakeLine{ebook, 1})
		s := c.start()
		require.Equal(t, "yoco", s.Payment.Gateway)

		body := pay(cardVisa)
		body["payment_gateway"] = "paypal"
		var problem map[string]any
		require.Equal(t, http.StatusBadRequest, c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/confirm",
			body, &problem, httpkit.HeaderIdempotencyKey, "switch-bad"))
		assert.Contains(t, fmt.Sprint(problem["errors"]), "payment_gateway")

		body["payment_gateway"] = "stripe"
		code, s := c.confirm(s.SessionID, "switch", body)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.COMPLETED, s.State)
		assert.Equal(t, "stripe", s.Payment.Gateway)
		assert.Equal(t, api.AuthCapture, s.Payment.Flow, "Variant A")
		assert.Equal(t, []string{"authorize S:authorize", "orders S:order", "capture S:capture", "orders.confirm S:confirm"},
			gw.callsFor(s.SessionID.String()))
	})

	t.Run("Variant B: an order failure moves no money", func(t *testing.T) {
		yoco(t)
		gw.failNext("orders", 3)
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		code, s := c.confirm(s.SessionID, "b-order", hosted)
		require.Equal(t, http.StatusUnprocessableEntity, code)
		assert.Equal(t, api.ORDERCREATE, s.Failure.State)
		assert.Nil(t, gw.txFor(s.SessionID.String()), "never charged, so nothing to refund (ADR-019 rule 3)")
		assert.Equal(t, before, gw.stock(mug))
		assert.NotContains(t, compensation(t, s.SessionID), domain.UndoPayment)
	})

	t.Run("Variant B: a failed hosted payment compensates", func(t *testing.T) {
		yoco(t)
		c := customer(t, fakeLine{mug, 1})
		before := gw.stock(mug)
		s := c.start()
		_, s = c.confirm(s.SessionID, "b-fail", hosted)
		require.Equal(t, api.CHARGING, s.State)
		gw.settle(s.SessionID.String(), false)
		require.NoError(t, a.Service.PaymentSettled(ctx, shop, s.SessionID.String(), domain.StateCharging))
		s = c.get(s.SessionID)
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "payment_declined", s.Failure.Reason)
		assert.Equal(t, "abandoned", gw.orderFor(s.SessionID.String()).Status)
		assert.Equal(t, before, gw.stock(mug))
	})

	t.Run("Variant B: cancel leaves the page open; a late payment is refunded", func(t *testing.T) {
		yoco(t)
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		_, s = c.confirm(s.SessionID, "b-late", hosted)
		require.Equal(t, api.CHARGING, s.State)
		require.Equal(t, http.StatusOK, c.do(http.MethodPost, "/checkout/"+s.SessionID.String()+"/cancel", nil, &s))
		assert.Equal(t, api.FAILED, s.State)
		assert.Equal(t, "cancelled", s.Failure.Reason)
		sid := s.SessionID.String()
		assert.Equal(t, "left_open", compensation(t, s.SessionID)[domain.UndoPayment], "Yoco cannot cancel a checkout")
		assert.Equal(t, "abandoned", gw.orderFor(sid).Status)

		tx := gw.settle(sid, true)
		require.NoError(t, a.Service.Charged(ctx, shop, sid, tx))
		assert.Equal(t, "refunded", gw.txFor(sid).Status)
		assert.Equal(t, "completed", compensation(t, s.SessionID)[domain.UndoLateCharge])
		require.NoError(t, a.Service.Charged(ctx, shop, sid, tx))
		assert.Equal(t, []string{"charge S:charge", "void S:charge:undo", "refund S:charge:refund"},
			filter(gw.callsFor(sid), "charge", "void", "refund"), "refunded once")
		assert.Equal(t, api.FAILED, c.get(s.SessionID).State)
	})

	t.Run("Variant B: a charge paid before expiry compensates forward", func(t *testing.T) {
		yoco(t)
		c := customer(t, fakeLine{mug, 1})
		s := c.start()
		_, s = c.confirm(s.SessionID, "b-exp", hosted)
		require.Equal(t, api.CHARGING, s.State)
		gw.settle(s.SessionID.String(), true)
		inShop(`UPDATE checkout_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.SessionID)
		_, err := a.Service.Expire(ctx, shop, s.SessionID.String())
		require.NoError(t, err)
		s = c.get(s.SessionID)
		assert.Equal(t, api.COMPLETED, s.State, "the void finds the charge: forward, not refunded")
		assert.Equal(t, "captured", gw.txFor(s.SessionID.String()).Status)
	})

	t.Run("start needs a gateway for the store currency", func(t *testing.T) {
		gw.set(func() { gw.routes = nil })
		t.Cleanup(func() { gw.set(func() { gw.routes = []fakeRoute{stripeRoute} }) })
		c := customer(t, fakeLine{ebook, 1})
		var problem map[string]any
		require.Equal(t, http.StatusConflict, c.do(http.MethodPost, "/checkout", map[string]any{}, &problem))
		assert.Contains(t, problem["detail"], "no payments in ZAR")
	})

	t.Run("admin lists and internal reads", func(t *testing.T) {
		admin := as(t, srv, "admin", "shop_admin")
		var page api.SessionList
		require.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/admin/sessions?state=COMPLETED&page_size=2", nil, &page))
		assert.Len(t, page.Items, 2)
		require.NotEmpty(t, page.Page.NextCursor)
		var next api.SessionList
		require.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/admin/sessions?state=COMPLETED&page_size=2&cursor="+page.Page.NextCursor, nil, &next))
		assert.NotEmpty(t, next.Items)
		assert.NotEqual(t, page.Items[0].SessionID, next.Items[0].SessionID)

		svc := as(t, srv, "yc-orders", "service")
		svc.hdr[authctx.HeaderCallerService] = "yc-orders"
		var s api.AdminSession
		require.Equal(t, http.StatusOK, svc.do(http.MethodGet, "/internal/sessions/"+page.Items[0].SessionID.String(), nil, &s))
		assert.Equal(t, api.COMPLETED, s.State)

		other := as(t, srv, "admin", "shop_admin")
		other.hdr[authctx.HeaderTenantID] = "shop-b"
		assert.Equal(t, http.StatusNotFound, other.do(http.MethodGet, "/admin/sessions/"+s.SessionID.String(), nil, nil))
	})
}
