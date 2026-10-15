//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-review/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/app"
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
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		c.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, b)
	} else if out != nil {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func ptr[T any](v T) *T { return &v }

func TestReviews(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-review")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	gateway := newOrdersGateway(t)
	t.Setenv("YC_REVIEW_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gateway.URL)
	t.Setenv("YC_REVIEW_SERVICE_SECRET", "review-secret-0123456789-0123456789")
	t.Setenv("YC_REVIEW__PURCHASE_CHECK__WINDOW", "1s")
	t.Setenv("YC_REVIEW__PURCHASE_CHECK__RETRY_INTERVAL", "10ms")
	t.Setenv("YC_REVIEW_PROMPT_DELAY", "0s")
	t.Setenv("YC_REVIEW_SWEEP_INTERVAL", "1h")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-review", Version: "test"})
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
			WHERE envelope->>'event_type' = $1 ORDER BY created_at, id`, eventType)
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
	last := func(eventType string) map[string]any {
		t.Helper()
		all := outbox(eventType)
		require.NotEmpty(t, all, eventType)
		return all[len(all)-1]
	}
	stats := func(p map[string]any) map[string]any {
		s, _ := p["product_stats"].(map[string]any)
		return s
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-orders", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.OrderStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })

	mug, kettle, teapot := uuid.NewString(), uuid.NewString(), uuid.NewString()
	thandi, sipho := uuid.NewString(), uuid.NewString()
	firstOrder := order{
		OrderID: uuid.NewString(), OrderNumber: "YC-1001", UserID: thandi, UserType: "customer",
		Items: []orderItem{{ProductID: mug, NameSnapshot: "Mug"}, {ProductID: kettle, NameSnapshot: "Kettle"}, {ProductID: mug, NameSnapshot: "Mug"}},
	}
	gateway.addOrder(firstOrder)

	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	customer := as(t, srv, "shop-a", thandi, "customer")
	other := as(t, srv, "shop-a", sipho, "customer")
	visitor := as(t, srv, "shop-a", uuid.NewString(), "visitor")
	otherShop := as(t, srv, "shop-b", uuid.NewString(), "shop_admin")

	var mugReview api.Review
	t.Run("customers review what they bought", func(t *testing.T) {
		var set api.ReviewSettings
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/review-settings", nil, &set))
		assert.Equal(t, api.ModerationMode("manual"), set.ModerationMode)
		assert.True(t, set.RequirePurchase)
		assert.Nil(t, set.UpdatedAt, "defaults until the shop sets its own")

		in := api.ReviewInput{ProductID: uuid.MustParse(mug), Rating: 5, Title: ptr(" Great mug "), Body: ptr("Holds a lot of tea.")}
		assert.Equal(t, http.StatusForbidden, visitor.do("POST", "/reviews", in, nil))
		assert.Equal(t, http.StatusBadRequest, customer.do("POST", "/reviews", api.ReviewInput{ProductID: in.ProductID, Rating: 6}, nil))
		assert.Equal(t, http.StatusConflict, other.do("POST", "/reviews", in, nil), "not bought")

		require.Equal(t, http.StatusCreated, customer.do("POST", "/reviews", in, &mugReview))
		assert.Equal(t, api.ReviewStatus("pending"), mugReview.Status)
		assert.True(t, mugReview.VerifiedPurchase)
		assert.False(t, mugReview.PurchasePending)
		assert.Equal(t, "Great mug", *mugReview.Title)
		assert.Equal(t, http.StatusConflict, customer.do("POST", "/reviews", in, nil), "one review per product")

		sub := last(events.TypeReviewSubmittedV1)
		assert.Equal(t, "pending", sub["status"])
		assert.Equal(t, true, sub["verified_purchase"])
		assert.EqualValues(t, 0, stats(sub)["rating_count"])

		var public api.ReviewList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/reviews?product_id="+mug, nil, &public))
		assert.Empty(t, public.Items, "pending reviews are not shown")
		var mine api.ReviewList
		require.Equal(t, http.StatusOK, customer.do("GET", "/reviews/mine", nil, &mine))
		require.Len(t, mine.Items, 1)
		assert.Equal(t, thandi, *mine.Items[0].UserID)
	})

	t.Run("admins moderate and respond", func(t *testing.T) {
		var pending api.ReviewList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/reviews?status=pending", nil, &pending))
		require.Len(t, pending.Items, 1)
		assert.Equal(t, http.StatusForbidden, customer.do("POST", "/admin/reviews/"+mugReview.ReviewID.String()+"/approve", nil, nil))
		assert.Equal(t, http.StatusNotFound, otherShop.do("POST", "/admin/reviews/"+mugReview.ReviewID.String()+"/approve", nil, nil))

		var r api.Review
		require.Equal(t, http.StatusOK, admin.do("POST", "/admin/reviews/"+mugReview.ReviewID.String()+"/approve", nil, &r))
		assert.Equal(t, api.ReviewStatus("approved"), r.Status)
		approved := last(events.TypeReviewApprovedV1)
		assert.EqualValues(t, 1, stats(approved)["rating_count"])
		assert.EqualValues(t, 5, stats(approved)["rating_sum"])

		var public api.ReviewList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/reviews?product_id="+mug, nil, &public))
		require.Len(t, public.Items, 1)
		assert.Nil(t, public.Items[0].UserID, "the public list leaves out the author")
		var st api.ReviewStats
		require.Equal(t, http.StatusOK, visitor.do("GET", "/reviews/stats?product_id="+mug, nil, &st))
		assert.EqualValues(t, 1, st.RatingCount)
		assert.InDelta(t, 5.0, st.RatingAverage, 1e-9)
		assert.Equal(t, []int64{0, 0, 0, 0, 1}, st.Distribution)

		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/reviews/"+mugReview.ReviewID.String()+"/response",
			map[string]string{"response": "Thank you!"}, &r))
		assert.Equal(t, "Thank you!", *r.ShopResponse)
		assert.Len(t, outbox(events.TypeReviewRespondedV1), 1)
	})

	t.Run("edits go back to moderation", func(t *testing.T) {
		var r api.Review
		require.Equal(t, http.StatusOK, customer.do("PUT", "/reviews/"+mugReview.ReviewID.String(),
			api.ReviewContent{Rating: 3, Body: ptr("Chipped after a week.")}, &r))
		assert.Equal(t, api.ReviewStatus("pending"), r.Status)
		assert.Nil(t, r.Title)
		edited := last(events.TypeReviewEditedV1)
		assert.Equal(t, "pending", edited["status"])
		assert.EqualValues(t, 0, stats(edited)["rating_count"], "a pending review leaves the stats")
		assert.Equal(t, http.StatusNotFound, other.do("PUT", "/reviews/"+mugReview.ReviewID.String(), api.ReviewContent{Rating: 1}, nil))

		require.Equal(t, http.StatusOK, admin.do("POST", "/admin/reviews/"+mugReview.ReviewID.String()+"/reject",
			map[string]string{"reason": "Mentions a competitor"}, &r))
		assert.Equal(t, api.ReviewStatus("rejected"), r.Status)
		assert.Equal(t, "Mentions a competitor", last(events.TypeReviewRejectedV1)["reason"])
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/reviews/"+mugReview.ReviewID.String()+"/reject",
			map[string]string{"reason": " "}, nil))

		var set api.ReviewSettings
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/review-settings",
			api.ReviewSettingsInput{ModerationMode: "auto", RequirePurchase: true, AllowEdit: true}, &set))
		require.NotNil(t, set.UpdatedAt)
		r = api.Review{}
		require.Equal(t, http.StatusOK, customer.do("PUT", "/reviews/"+mugReview.ReviewID.String(),
			api.ReviewContent{Rating: 4, Title: ptr("Good after all")}, &r))
		assert.Equal(t, api.ReviewStatus("approved"), r.Status, "auto-approved")
		assert.Empty(t, r.RejectionReason)
		assert.EqualValues(t, 4, stats(last(events.TypeReviewEditedV1))["rating_sum"])
	})

	t.Run("customers like products", func(t *testing.T) {
		var st api.ProductStats
		assert.Equal(t, http.StatusForbidden, visitor.do("PUT", "/likes/"+mug, nil, nil))
		require.Equal(t, http.StatusOK, customer.do("PUT", "/likes/"+mug, nil, &st))
		assert.EqualValues(t, 1, st.LikeCount)
		assert.EqualValues(t, 1, st.RatingCount)
		require.Equal(t, http.StatusOK, customer.do("PUT", "/likes/"+mug, nil, &st))
		assert.EqualValues(t, 1, st.LikeCount, "liking again changes nothing")
		require.Equal(t, http.StatusOK, other.do("PUT", "/likes/"+teapot, nil, &st))
		assert.Len(t, outbox(events.TypeReviewLikeAddedV1), 2)

		var likes api.LikeList
		require.Equal(t, http.StatusOK, customer.do("GET", "/likes", nil, &likes))
		require.Len(t, likes.Items, 1)
		assert.Equal(t, mug, likes.Items[0].ProductID.String())

		require.Equal(t, http.StatusOK, customer.do("DELETE", "/likes/"+mug, nil, &st))
		assert.EqualValues(t, 0, st.LikeCount)
		require.Equal(t, http.StatusOK, customer.do("DELETE", "/likes/"+mug, nil, &st))
		removed := outbox(events.TypeReviewLikeRemovedV1)
		require.Len(t, removed, 1)
		assert.EqualValues(t, 0, stats(removed[0])["like_count"])
		assert.Greater(t, stats(removed[0])["version"], stats(last(events.TypeReviewEditedV1))["version"], "versions grow")
	})

	t.Run("purchases yc-orders cannot confirm are checked again", func(t *testing.T) {
		gateway.setDown(true)
		var r api.Review
		require.Equal(t, http.StatusCreated, other.do("POST", "/reviews",
			api.ReviewInput{ProductID: uuid.MustParse(kettle), Rating: 4}, &r))
		assert.Equal(t, api.ReviewStatus("pending"), r.Status, "waits even in auto mode")
		assert.True(t, r.PurchasePending)
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/reviews/"+r.ReviewID.String()+"/approve", nil, nil))

		gateway.addOrder(order{
			OrderID: uuid.NewString(), OrderNumber: "YC-1002", UserID: sipho, UserType: "customer",
			Items: []orderItem{{ProductID: kettle, NameSnapshot: "Kettle"}},
		})
		gateway.setDown(false)
		time.Sleep(20 * time.Millisecond)
		n, err := a.Service.CheckPurchases(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/reviews/"+r.ReviewID.String(), nil, &r))
		assert.Equal(t, api.ReviewStatus("approved"), r.Status)
		assert.True(t, r.VerifiedPurchase)
		assert.False(t, r.PurchasePending)
		assert.Equal(t, r.ReviewID.String(), last(events.TypeReviewApprovedV1)["review_id"])

		// Not bought after all.
		gateway.setDown(true)
		var teapotReview api.Review
		require.Equal(t, http.StatusCreated, other.do("POST", "/reviews",
			api.ReviewInput{ProductID: uuid.MustParse(teapot), Rating: 1}, &teapotReview))
		gateway.setDown(false)
		time.Sleep(20 * time.Millisecond)
		_, err = a.Service.CheckPurchases(ctx)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/reviews/"+teapotReview.ReviewID.String(), nil, &r))
		assert.Equal(t, api.ReviewStatus("rejected"), r.Status)
		assert.Equal(t, "unverified purchase", *r.RejectionReason)
		assert.Equal(t, http.StatusConflict, other.do("PUT", "/reviews/"+teapotReview.ReviewID.String(), api.ReviewContent{Rating: 5}, nil))

		// yc-orders stays down past the window.
		gateway.setDown(true)
		var kettleReview api.Review
		require.Equal(t, http.StatusCreated, customer.do("POST", "/reviews",
			api.ReviewInput{ProductID: uuid.MustParse(kettle), Rating: 2}, &kettleReview))
		time.Sleep(1100 * time.Millisecond)
		_, err = a.Service.CheckPurchases(ctx)
		require.NoError(t, err)
		gateway.setDown(false)
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/reviews/"+kettleReview.ReviewID.String(), nil, &r))
		assert.Equal(t, api.ReviewStatus("rejected"), r.Status)
		assert.Equal(t, "purchase could not be verified", *r.RejectionReason)
	})

	t.Run("delivered orders prompt customers to review", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, customer.do("DELETE", "/reviews/"+mustID(t, customer, kettle), nil, nil))
		deleted := last(events.TypeReviewDeletedV1)
		assert.Equal(t, kettle, deleted["product_id"])

		guestOrder := order{
			OrderID: uuid.NewString(), OrderNumber: "YC-1003", UserID: uuid.NewString(), UserType: "guest",
			Items: []orderItem{{ProductID: mug, NameSnapshot: "Mug"}},
		}
		gateway.addOrder(guestOrder)
		for _, o := range []order{firstOrder, guestOrder} {
			require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
				EventType: events.TypeOrderDeliveredV1, TenantID: "shop-a", Actor: eventbus.Actor{Type: "service", ID: "yc-orders"},
				Payload: events.OrderDeliveredV1{
					OrderID: o.OrderID, OrderNumber: o.OrderNumber, UserID: o.UserID, DeliveredAt: time.Now().UTC(),
					Items: []events.OrderDeliveredV1ItemsItem{{SKUID: uuid.NewString(), ProductID: ptr(mug), Quantity: 1}},
				},
			}))
		}
		require.Eventually(t, func() bool {
			if _, err := a.Service.SendPrompts(ctx); err != nil {
				t.Log(err)
				return false
			}
			var open, done int
			err := dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE done_at IS NULL), COUNT(*) FILTER (WHERE done_at IS NOT NULL)
					FROM review_prompts`).Scan(&open, &done)
			})
			return err == nil && open == 0 && done == 2
		}, 10*time.Second, 100*time.Millisecond)

		prompts := outbox(events.TypeReviewPromptRequestedV1)
		require.Len(t, prompts, 1, "guests are not prompted")
		assert.Equal(t, thandi, prompts[0]["user_id"])
		assert.Equal(t, "YC-1001", prompts[0]["order_number"])
		products, _ := prompts[0]["products"].([]any)
		require.Len(t, products, 1, "only the products not reviewed yet")
		assert.Equal(t, map[string]any{"product_id": kettle, "name": "Kettle"}, products[0])
	})
}

// mustID returns the client's review of a product.
func mustID(t *testing.T, c client, productID string) string {
	t.Helper()
	var mine api.ReviewList
	require.Equal(t, http.StatusOK, c.do("GET", "/reviews/mine?product_id="+productID, nil, &mine))
	require.Len(t, mine.Items, 1)
	return mine.Items[0].ReviewID.String()
}
