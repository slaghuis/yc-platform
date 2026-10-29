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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-support/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/app"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/service"
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
	req.Header.Set("Content-Type", "application/json")
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

func newInput(topic, email, body string) map[string]any {
	return map[string]any{"topic": topic, "contact_email": email, "contact_name": "Test Person", "body": body}
}

func TestSupport(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-support")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_SUPPORT_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gwSrv.URL)
	t.Setenv("YC_SUPPORT_SERVICE_SECRET", "support-secret")
	t.Setenv("YC_SUPPORT_STOREFRONT_BASE", "https://{tenant_id}.shops.test")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 72*time.Hour, cfg.Support.IdleTTL)
	assert.Equal(t, time.Minute, cfg.Support.SweepInterval)
	cfg.Support.SweepInterval = time.Hour // the test sweeps by hand
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-support", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	testkit.AssertTenantIsolation(t, db, "outbox")

	inTenant := func(tenant string, fn func(tx pgx.Tx)) {
		t.Helper()
		require.NoError(t, pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant)
			require.NoError(t, err)
			fn(tx)
			return nil
		}))
	}
	backdate := func(tenant, id string, by time.Duration) {
		inTenant(tenant, func(tx pgx.Tx) {
			_, err := tx.Exec(ctx, `UPDATE support_requests SET last_activity_at = last_activity_at - $3::interval
				WHERE tenant_id = $1 AND id = $2`, tenant, id, by.String())
			require.NoError(t, err)
		})
	}
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

	adminA := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	adminB := as(t, srv, "shop-b", uuid.NewString(), "shop_admin")
	alice, bob, carol := uuid.NewString(), uuid.NewString(), uuid.NewString()
	aliceC := as(t, srv, "shop-a", alice, "customer")
	bobC := as(t, srv, "shop-a", bob, "customer")
	visitor := as(t, srv, "shop-a", uuid.NewString(), "visitor")

	var visitorThread, aliceThread api.Thread

	t.Run("a visitor opens a request and gets a reference", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, visitor.do("POST", "/support/requests",
			newInput("allergen", " Dana@Example.com ", "Is the carrot cake nut free?"), &visitorThread))
		r := visitorThread.Request
		assert.Regexp(t, `^SUP-[A-Z0-9]{6}$`, r.Reference)
		assert.Equal(t, api.Status("new"), r.Status)
		assert.Equal(t, api.UserType("visitor"), r.UserType)
		assert.Equal(t, "dana@example.com", r.ContactEmail, "normalised")
		assert.Nil(t, r.UserID)
		require.Len(t, visitorThread.Messages, 1)
		assert.Equal(t, api.AuthorType("visitor"), visitorThread.Messages[0].AuthorType)

		got := gw.reset()
		require.Len(t, got, 2)
		var ack, shop sent
		for _, m := range got {
			if m.typ() == service.MsgAck {
				ack = m
			} else {
				shop = m
			}
		}
		assert.Equal(t, "email", ack.str("channel"))
		assert.Equal(t, "dana@example.com", ack.str("address"))
		assert.Empty(t, ack.str("user_id"))
		assert.NotContains(t, ack.context(), "support_url", "visitors get no storefront link")
		assert.Equal(t, r.Reference, ack.context()["reference"])
		assert.Equal(t, "allergen", ack.context()["topic"])
		assert.Equal(t, service.MsgNewRequest, shop.typ())
		assert.Equal(t, "support@shop-a.test", shop.str("address"), "the tenant's contact email")
		assert.Equal(t, "email", shop.str("channel"))
		assert.NotEqual(t, ack.ref(), shop.ref())
		assert.Len(t, outbox("shop-a", events.TypeSupportRequestCreatedV1), 1)
	})

	t.Run("a visitor cannot list, read or reply until claimed", func(t *testing.T) {
		id := visitorThread.Request.ID.String()
		assert.Equal(t, http.StatusForbidden, visitor.do("GET", "/support/requests", nil, nil))
		assert.Equal(t, http.StatusForbidden, visitor.do("GET", "/support/requests/"+id, nil, nil))
		assert.Equal(t, http.StatusForbidden, visitor.do("POST", "/support/requests/"+id+"/messages",
			map[string]any{"body": "hello?"}, nil))
	})

	t.Run("a customer opens a request", func(t *testing.T) {
		in := newInput("order", "alice@example.com", "Where is my order?")
		in["order_id"] = uuid.NewString()
		in["page_url"] = "https://shop-a.test/account/orders"
		require.Equal(t, http.StatusCreated, aliceC.do("POST", "/support/requests", in, &aliceThread))
		r := aliceThread.Request
		assert.Equal(t, api.UserType("customer"), r.UserType)
		require.NotNil(t, r.UserID)
		assert.Equal(t, alice, r.UserID.String())
		assert.NotEqual(t, visitorThread.Request.Reference, r.Reference)

		got := gw.reset()
		require.Len(t, got, 2)
		for _, m := range got {
			if m.typ() == service.MsgAck {
				assert.Equal(t, alice, m.str("user_id"), "customers are addressed by account")
				assert.Empty(t, m.str("address"))
				assert.Equal(t, "https://shop-a.shops.test/account/support/"+r.ID.String(), m.context()["support_url"])
			} else {
				assert.NotContains(t, m.context(), "support_url", "the shop's copy has no customer link")
			}
		}
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/support/requests", newInput("refund", "a@b.co", "x"), nil))
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/support/requests", newInput("order", "not-an-email", "x"), nil))
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/support/requests", newInput("order", "a@b.co", "  "), nil))
		assert.Equal(t, http.StatusForbidden, adminA.do("POST", "/support/requests", newInput("order", "a@b.co", "x"), nil),
			"staff do not open requests")
	})

	t.Run("the shop lists and filters requests", func(t *testing.T) {
		var list api.RequestList
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests", nil, &list))
		assert.Len(t, list.Items, 2)
		assert.Equal(t, aliceThread.Request.ID, list.Items[0].ID, "most recent first")

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?topic=allergen", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, visitorThread.Request.ID, list.Items[0].ID)

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?status=open_shop", nil, &list))
		assert.Empty(t, list.Items)

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?email=ALICE@example.com", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, aliceThread.Request.ID, list.Items[0].ID)

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?reference="+visitorThread.Request.Reference, nil, &list))
		require.Len(t, list.Items, 1)

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?page_size=1", nil, &list))
		require.Len(t, list.Items, 1)
		require.NotEmpty(t, list.Page.NextCursor)
		var next api.RequestList
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests?page_size=1&cursor="+list.Page.NextCursor, nil, &next))
		require.Len(t, next.Items, 1)
		assert.Equal(t, visitorThread.Request.ID, next.Items[0].ID)
		assert.Empty(t, next.Page.NextCursor)

		assert.Equal(t, http.StatusBadRequest, adminA.do("GET", "/admin/support/requests?status=bogus", nil, nil))
		assert.Equal(t, http.StatusBadRequest, adminA.do("GET", "/admin/support/requests?cursor=!!", nil, nil))

		var other api.RequestList
		require.Equal(t, http.StatusOK, adminB.do("GET", "/admin/support/requests", nil, &other))
		assert.Empty(t, other.Items, "another shop sees nothing")
		assert.Equal(t, http.StatusNotFound, adminB.do("GET", "/admin/support/requests/"+aliceThread.Request.ID.String(), nil, nil))
		assert.Equal(t, http.StatusNotFound, adminB.do("POST", "/admin/support/requests/"+aliceThread.Request.ID.String()+"/messages",
			map[string]any{"body": "hi"}, nil))
		assert.Equal(t, http.StatusNotFound, adminB.do("POST", "/admin/support/requests/"+aliceThread.Request.ID.String()+"/close", nil, nil))
		assert.Equal(t, http.StatusForbidden, aliceC.do("GET", "/admin/support/requests", nil, nil))
	})

	t.Run("the shop answers: new moves to open_customer and the requester is told", func(t *testing.T) {
		gw.reset()
		var m api.Message
		path := "/admin/support/requests/" + aliceThread.Request.ID.String()
		require.Equal(t, http.StatusCreated, adminA.do("POST", path+"/messages", map[string]any{"body": "It ships today."}, &m))
		assert.Equal(t, api.AuthorType("shop_admin"), m.AuthorType)
		require.NotNil(t, m.AuthorUserID)

		var th api.Thread
		require.Equal(t, http.StatusOK, adminA.do("GET", path, nil, &th))
		assert.Equal(t, api.Status("open_customer"), th.Request.Status)
		require.Len(t, th.Messages, 2)
		assert.Equal(t, "Where is my order?", th.Messages[0].Body, "oldest first")

		got := gw.reset()
		require.Len(t, got, 1)
		assert.Equal(t, service.MsgShopReply, got[0].typ())
		assert.Equal(t, alice, got[0].str("user_id"))
		assert.Equal(t, "It ships today.", got[0].context()["excerpt"])
		assert.Contains(t, got[0].context()["support_url"], "/account/support/"+aliceThread.Request.ID.String())

		assert.Equal(t, http.StatusBadRequest, adminA.do("POST", path+"/messages", map[string]any{"body": " "}, nil))
	})

	t.Run("the shop answers a visitor by email without a link", func(t *testing.T) {
		gw.reset()
		path := "/admin/support/requests/" + visitorThread.Request.ID.String()
		require.Equal(t, http.StatusCreated, adminA.do("POST", path+"/messages", map[string]any{"body": "Yes, it is nut free."}, nil))
		got := gw.reset()
		require.Len(t, got, 1)
		assert.Equal(t, service.MsgShopReply, got[0].typ())
		assert.Equal(t, "dana@example.com", got[0].str("address"))
		assert.NotContains(t, got[0].context(), "support_url")
	})

	t.Run("a customer sees only their own requests", func(t *testing.T) {
		var list api.RequestList
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, aliceThread.Request.ID, list.Items[0].ID)

		list = api.RequestList{}
		require.Equal(t, http.StatusOK, bobC.do("GET", "/support/requests", nil, &list))
		assert.Empty(t, list.Items)

		id := aliceThread.Request.ID.String()
		var th api.Thread
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests/"+id, nil, &th))
		assert.Len(t, th.Messages, 2)
		assert.Equal(t, http.StatusNotFound, bobC.do("GET", "/support/requests/"+id, nil, nil), "not theirs")
		assert.Equal(t, http.StatusNotFound, bobC.do("POST", "/support/requests/"+id+"/messages", map[string]any{"body": "hi"}, nil))
		assert.Equal(t, http.StatusNotFound, aliceC.do("GET", "/support/requests/"+uuid.NewString(), nil, nil))
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", alice, "customer").do("GET", "/support/requests/"+id, nil, nil))
	})

	t.Run("the customer replies: open_customer moves to open_shop and the shop is told", func(t *testing.T) {
		gw.reset()
		id := aliceThread.Request.ID.String()
		require.Equal(t, http.StatusCreated, aliceC.do("POST", "/support/requests/"+id+"/messages",
			map[string]any{"body": "Thanks, when will it arrive?"}, nil))
		var th api.Thread
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests/"+id, nil, &th))
		assert.Equal(t, api.Status("open_shop"), th.Request.Status)
		require.Len(t, th.Messages, 3)

		got := gw.reset()
		require.Len(t, got, 1)
		assert.Equal(t, service.MsgCustomerReply, got[0].typ())
		assert.Equal(t, "support@shop-a.test", got[0].str("address"))

		require.Equal(t, http.StatusCreated, adminA.do("POST", "/admin/support/requests/"+id+"/messages", map[string]any{"body": "Tomorrow."}, nil))
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests/"+id, nil, &th))
		assert.Equal(t, api.Status("open_customer"), th.Request.Status, "open_shop → open_customer")
		assert.Equal(t, http.StatusBadRequest, aliceC.do("POST", "/support/requests/"+id+"/messages", map[string]any{"body": ""}, nil))
	})

	t.Run("a failing notification does not fail the request", func(t *testing.T) {
		gw.reset()
		gw.setFailSend(true)
		t.Cleanup(func() { gw.setFailSend(false) })
		id := aliceThread.Request.ID.String()
		assert.Equal(t, http.StatusCreated, aliceC.do("POST", "/support/requests/"+id+"/messages",
			map[string]any{"body": "Great, thank you."}, nil))
		assert.Empty(t, gw.messages())
	})

	t.Run("closing is final", func(t *testing.T) {
		gw.setFailSend(false)
		gw.reset()
		id := aliceThread.Request.ID.String()
		var closed api.SupportRequest
		require.Equal(t, http.StatusOK, adminA.do("POST", "/admin/support/requests/"+id+"/close", nil, &closed))
		assert.Equal(t, api.Status("closed"), closed.Status)
		require.NotNil(t, closed.ClosedReason)
		assert.Equal(t, api.ClosedReason("staff"), *closed.ClosedReason)
		require.NotNil(t, closed.ClosedAt)

		got := gw.reset()
		require.Len(t, got, 2, "the customer and the shop")
		audiences := map[any]string{}
		for _, m := range got {
			assert.Equal(t, service.MsgClosed, m.typ())
			assert.Equal(t, "staff", m.context()["closed_reason"])
			audiences[m.context()["audience"]] = m.ref()
		}
		assert.Len(t, audiences, 2)
		assert.Contains(t, audiences, "customer")
		assert.Contains(t, audiences, "shop")

		assert.Equal(t, http.StatusConflict, adminA.do("POST", "/admin/support/requests/"+id+"/close", nil, nil))
		assert.Equal(t, http.StatusConflict, adminA.do("POST", "/admin/support/requests/"+id+"/messages", map[string]any{"body": "hi"}, nil))
		assert.Equal(t, http.StatusConflict, aliceC.do("POST", "/support/requests/"+id+"/messages", map[string]any{"body": "hi"}, nil))
		assert.Empty(t, gw.messages(), "refused posts notify nobody")

		var th api.Thread
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests/"+id, nil, &th), "closed threads stay readable")
		assert.Equal(t, api.Status("closed"), th.Request.Status)
		assert.Len(t, outbox("shop-a", events.TypeSupportRequestClosedV1), 1)
	})

	t.Run("registering with the same email claims the visitor's request", func(t *testing.T) {
		claim := func(c client, user, email string) (int, api.ClaimResult) {
			var res api.ClaimResult
			status := c.do("POST", "/internal/support/requests/claim", map[string]any{"user_id": user, "email": email}, &res)
			return status, res
		}
		svc := as(t, srv, "shop-a", "yc-identity", "service")
		svc.hdr[authctx.HeaderCallerService] = "yc-identity"

		status, _ := claim(aliceC, carol, "dana@example.com")
		assert.Equal(t, http.StatusUnauthorized, status, "services only")
		status, _ = claim(svc, "nope", "dana@example.com")
		assert.Equal(t, http.StatusBadRequest, status)
		status, _ = claim(svc, carol, "nope")
		assert.Equal(t, http.StatusBadRequest, status)

		other := as(t, srv, "shop-b", "yc-identity", "service")
		other.hdr[authctx.HeaderCallerService] = "yc-identity"
		status, res := claim(other, carol, "dana@example.com")
		assert.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 0, res.Claimed, "claims are per shop")

		status, res = claim(svc, carol, " Dana@EXAMPLE.com")
		assert.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 1, res.Claimed)
		status, res = claim(svc, carol, "dana@example.com")
		assert.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 0, res.Claimed, "once")
		status, res = claim(svc, bob, "alice@example.com")
		assert.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 0, res.Claimed, "alice's own request already has an owner")

		carolC := as(t, srv, "shop-a", carol, "customer")
		var list api.RequestList
		require.Equal(t, http.StatusOK, carolC.do("GET", "/support/requests", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, visitorThread.Request.ID, list.Items[0].ID)
		assert.Equal(t, api.UserType("customer"), list.Items[0].UserType)

		gw.reset()
		id := visitorThread.Request.ID.String()
		require.Equal(t, http.StatusCreated, carolC.do("POST", "/support/requests/"+id+"/messages",
			map[string]any{"body": "Great, thanks"}, nil))
		var th api.Thread
		require.Equal(t, http.StatusOK, carolC.do("GET", "/support/requests/"+id, nil, &th))
		assert.Equal(t, api.Status("open_shop"), th.Request.Status)
		assert.Equal(t, http.StatusNotFound, bobC.do("GET", "/support/requests/"+id, nil, nil))
		assert.Equal(t, http.StatusForbidden, visitor.do("GET", "/support/requests/"+id, nil, nil), "the visitor session is still a visitor")
	})

	t.Run("the sweeper closes requests idle for 72 hours", func(t *testing.T) {
		var idle, fresh, closedAlready api.Thread
		require.Equal(t, http.StatusCreated, visitor.do("POST", "/support/requests", newInput("delivery", "erin@example.com", "Late again"), &idle))
		require.Equal(t, http.StatusCreated, visitor.do("POST", "/support/requests", newInput("payment", "fay@example.com", "Charged twice"), &fresh))
		require.Equal(t, http.StatusCreated, visitor.do("POST", "/support/requests", newInput("other", "gus@example.com", "Hi"), &closedAlready))
		require.Equal(t, http.StatusOK, adminA.do("POST", "/admin/support/requests/"+closedAlready.Request.ID.String()+"/close", nil, nil))
		backdate("shop-a", idle.Request.ID.String(), 73*time.Hour)
		backdate("shop-a", closedAlready.Request.ID.String(), 100*time.Hour)
		backdate("shop-a", fresh.Request.ID.String(), 71*time.Hour)
		gw.reset()

		n, err := a.Service.SweepIdle(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n, "closed and recent requests are not due")

		var th api.Thread
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests/"+idle.Request.ID.String(), nil, &th))
		assert.Equal(t, api.Status("closed"), th.Request.Status)
		require.NotNil(t, th.Request.ClosedReason)
		assert.Equal(t, api.ClosedReason("idle"), *th.Request.ClosedReason)
		require.Len(t, th.Messages, 2)
		assert.Equal(t, api.AuthorType("system"), th.Messages[1].AuthorType)
		assert.Contains(t, th.Messages[1].Body, "72 hours")

		got := gw.reset()
		require.Len(t, got, 2, "the visitor and the shop")
		for _, m := range got {
			assert.Equal(t, service.MsgClosed, m.typ())
			assert.Equal(t, "idle", m.context()["closed_reason"])
		}
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests/"+fresh.Request.ID.String(), nil, &th))
		assert.Equal(t, api.Status("new"), th.Request.Status)

		n, err = a.Service.SweepIdle(ctx)
		require.NoError(t, err)
		assert.Equal(t, 0, n, "nothing is swept twice")
		assert.Empty(t, gw.messages())

		byRequest := map[any]map[string]any{}
		for _, c := range outbox("shop-a", events.TypeSupportRequestClosedV1) {
			byRequest[c["request_id"]] = c
		}
		idleEvt := byRequest[idle.Request.ID.String()]
		require.NotNil(t, idleEvt)
		assert.Equal(t, "idle", idleEvt["closed_reason"])
		assert.NotContains(t, idleEvt, "closed_by")
		staffEvt := byRequest[closedAlready.Request.ID.String()]
		require.NotNil(t, staffEvt)
		assert.Equal(t, "staff", staffEvt["closed_reason"])
		assert.NotEmpty(t, staffEvt["closed_by"])
		assert.NotEmpty(t, outbox("shop-a", events.TypeSupportMessagePostedV1))
	})

	t.Run("a deleted user's threads are erased (GDPR)", func(t *testing.T) {
		validator, err := events.NewValidator()
		require.NoError(t, err)
		upstream, err := eventbus.NewClient(ctx, eventbus.Config{
			NATSURL: natsURL, ServiceName: "yc-identity", Validator: validator,
			Streams: eventbus.GDPRStreams(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = upstream.Close(context.Background()) })
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: events.TypeIdentityUserDeletedV1, TenantID: "shop-a",
			Actor: eventbus.Actor{Type: "service", ID: "yc-identity"},
			Payload: events.IdentityUserDeletedV1{
				UserID: alice, Pseudonym: uuid.NewString(), Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
			},
		}))
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		done := outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)[0]
		assert.Equal(t, "yc-support", done["service"])
		assert.Equal(t, "anonymized", done["action"])
		assert.EqualValues(t, 1+5, done["record_count"], "one request and its five messages")

		var th api.Thread
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests/"+aliceThread.Request.ID.String(), nil, &th))
		r := th.Request
		assert.Empty(t, r.ContactEmail)
		assert.Empty(t, r.ContactName)
		assert.Nil(t, r.ContactPhone)
		assert.Nil(t, r.PageURL)
		assert.Nil(t, r.UserID)
		assert.Equal(t, aliceThread.Request.Reference, r.Reference, "the shop keeps the reference and topic")
		assert.Equal(t, api.Topic("order"), r.Topic)
		for _, m := range th.Messages {
			assert.Equal(t, "[removed]", m.Body)
			if m.AuthorType == "customer" {
				assert.Nil(t, m.AuthorUserID)
			}
		}
		var list api.RequestList
		require.Equal(t, http.StatusOK, aliceC.do("GET", "/support/requests", nil, &list))
		assert.Empty(t, list.Items)

		var other api.Thread
		require.Equal(t, http.StatusOK, adminA.do("GET", "/admin/support/requests/"+visitorThread.Request.ID.String(), nil, &other))
		assert.Equal(t, "dana@example.com", other.Request.ContactEmail, "other users' threads are untouched")
		assert.NotEqual(t, "[removed]", other.Messages[0].Body)
	})

	t.Run("RLS keeps shops apart", func(t *testing.T) {
		inTenant("shop-b", func(tx pgx.Tx) {
			var n int
			require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM support_requests`).Scan(&n))
			assert.Zero(t, n)
			require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM support_messages`).Scan(&n))
			assert.Zero(t, n)
		})
	})
}
