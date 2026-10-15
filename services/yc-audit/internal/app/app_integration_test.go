//go:build integration

package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
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
	api "github.com/slaghuis/yc-platform/services/yc-audit/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   uuid.NewString(),
		authctx.HeaderActorType: actorType,
	}}
}

// get returns the status, the X-YC-Truncated header and the raw body.
func (c client) get(path string) (int, string, []byte) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.srv.URL+path, nil)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if resp.StatusCode >= 400 {
		c.t.Logf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return resp.StatusCode, resp.Header.Get("X-YC-Truncated"), body
}

func (c client) do(path string, out any) int {
	c.t.Helper()
	status, _, body := c.get(path)
	if status < 400 && out != nil {
		require.NoError(c.t, json.Unmarshal(body, out))
	}
	return status
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-audit")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_AUDIT_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_AUDIT_GDPR_DEADLINE", "2s")
	t.Setenv("YC_AUDIT_SWEEP_INTERVAL", "1h")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.GDPR.ExpectedServices = []string{"yc-preference", "yc-review"}
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-audit", Version: "test"})
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
	recorded := func(eventType string) int {
		t.Helper()
		var n int
		require.NoError(t, dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_records WHERE event_type = $1`, eventType).Scan(&n)
		}))
		return n
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
		Streams: append([]eventbus.StreamConfig{eventbus.PaymentStream(), eventbus.OrderStream()}, eventbus.GDPRStreams()...),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })
	publish := func(tenant, service string, payload interface{ EventType() string }) {
		t.Helper()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: payload.EventType(), TenantID: tenant,
			Actor: eventbus.Actor{Type: "service", ID: service}, Payload: payload,
		}))
	}

	sysadmin := as(t, srv, "_platform", "system_admin")
	shopA := as(t, srv, "shop-a", "shop_admin")
	shopB := as(t, srv, "shop-b", "shop_admin")
	customer := as(t, srv, "shop-a", "customer")

	t.Run("audited events are recorded once", func(t *testing.T) {
		now := time.Now().UTC()
		publish("shop-a", "yc-identity", events.IdentityUserPromotedV1{
			UserID: uuid.NewString(), FromRole: "customer", ToRole: "shop_admin", PromotedAt: now,
		})
		publish("shop-a", "yc-payment", events.PaymentChargebackReceivedV1{
			ChargebackID: uuid.NewString(), TransactionID: uuid.NewString(), Gateway: "stripe",
			GatewayReference: "dp_1", Amount: events.Money{AmountMinor: 12_500, Currency: "ZAR"},
			Reason: ptr("fraudulent"), ReceivedAt: now,
		})
		publish("shop-b", "yc-payment", events.PaymentWebhookSignatureFailedV1{
			Gateway: "stripe", Reason: "signature mismatch", ReceivedAt: now,
		})
		require.Eventually(t, func() bool {
			return recorded(events.TypeIdentityUserPromotedV1) == 1 &&
				recorded(events.TypePaymentChargebackReceivedV1) == 1 &&
				recorded(events.TypePaymentWebhookSignatureFailedV1) == 1
		}, 10*time.Second, 50*time.Millisecond)

		var all api.AuditRecordList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/audit", &all))
		assert.Len(t, all.Items, 3)
		var chargebacks api.AuditRecordList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/audit?category=financial&tenant_id=shop-a", &chargebacks))
		require.Len(t, chargebacks.Items, 1)
		cb := chargebacks.Items[0]
		assert.Equal(t, "yc-upstream", cb.SourceService)
		assert.NotEmpty(t, cb.Summary)

		var detail api.AuditRecord
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/audit/"+cb.ID.String(), &detail))
		require.NotNil(t, detail.Payload)
		assert.Equal(t, "dp_1", (*detail.Payload)["gateway_reference"])
		assert.Equal(t, http.StatusNotFound, sysadmin.do("/sysadmin/audit/"+uuid.NewString(), nil))

		var page api.AuditRecordList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/audit?page_size=2", &page))
		require.Len(t, page.Items, 2)
		require.NotEmpty(t, page.Page.NextCursor)
		var rest api.AuditRecordList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/audit?page_size=2&cursor="+page.Page.NextCursor, &rest))
		require.Len(t, rest.Items, 1)
	})

	t.Run("shop admins see their own shop only", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, shopA.do("/sysadmin/audit", nil))
		assert.Equal(t, http.StatusForbidden, customer.do("/admin/audit", nil))

		var mine api.AuditRecordList
		require.Equal(t, http.StatusOK, shopA.do("/admin/audit", &mine))
		require.Len(t, mine.Items, 2)
		for _, r := range mine.Items {
			assert.Equal(t, "shop-a", r.TenantID)
		}
		var theirs api.AuditRecordList
		require.Equal(t, http.StatusOK, shopB.do("/admin/audit", &theirs))
		require.Len(t, theirs.Items, 1)
		assert.Equal(t, api.Category("security"), theirs.Items[0].Category)
		assert.Equal(t, http.StatusForbidden, shopA.do("/admin/audit?category=gdpr", nil))
	})

	t.Run("records cannot be changed or removed", func(t *testing.T) {
		var canUpdate, canDelete, canTruncate, canInsert bool
		require.NoError(t, db.QueryRow(ctx, `SELECT
				has_table_privilege('audit_records', 'UPDATE'), has_table_privilege('audit_records', 'DELETE'),
				has_table_privilege('audit_records', 'TRUNCATE'), has_table_privilege('audit_records', 'INSERT')`).
			Scan(&canUpdate, &canDelete, &canTruncate, &canInsert))
		assert.False(t, canUpdate)
		assert.False(t, canDelete)
		assert.False(t, canTruncate)
		assert.True(t, canInsert)

		for _, stmt := range []string{
			`UPDATE audit_records SET summary = 'tampered'`,
			`DELETE FROM audit_records`,
			`TRUNCATE audit_records`,
		} {
			err := dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			require.Error(t, err, stmt)
			assert.Contains(t, err.Error(), "SQLSTATE 42501", stmt)
		}
		assert.Equal(t, 1, recorded(events.TypePaymentChargebackReceivedV1))
	})

	t.Run("exports are CSV or JSON lines", func(t *testing.T) {
		status, truncated, body := sysadmin.get("/sysadmin/audit/export")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "false", truncated)
		rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 4, "a header and three records")
		assert.Equal(t, "id", rows[0][0])

		status, _, body = sysadmin.get("/sysadmin/audit/export?format=jsonl&tenant_id=shop-a")
		require.Equal(t, http.StatusOK, status)
		var lines []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			var m map[string]any
			require.NoError(t, json.Unmarshal(sc.Bytes(), &m))
			lines = append(lines, m)
		}
		require.Len(t, lines, 2)
		assert.Equal(t, "shop-a", lines[0]["tenant_id"])

		status, _, _ = shopA.get("/sysadmin/audit/export")
		assert.Equal(t, http.StatusForbidden, status)
	})

	t.Run("a deletion completes when every service reports", func(t *testing.T) {
		user, pseudonym := uuid.NewString(), uuid.NewString()
		report := func(service string) {
			publish("shop-a", service, events.PlatformGdprCascadeCompletedV1{
				UserID: user, Pseudonym: pseudonym, Service: service, Action: "deleted", RecordCount: 2,
				CompletedAt: time.Now().UTC(),
			})
		}
		report("yc-preference")
		publish("shop-a", "yc-identity", events.IdentityUserDeletedV1{
			UserID: user, Pseudonym: pseudonym, Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			var e api.RegisterEntry
			return sysadmin.do("/sysadmin/gdpr/register/"+user, &e) == http.StatusOK && len(e.MissingServices) == 1
		}, 10*time.Second, 50*time.Millisecond, "a report before the deletion still counts")
		assert.Empty(t, outbox(events.TypeAuditGdprCascadeCompleteV1))

		report("yc-review")
		require.Eventually(t, func() bool { return len(outbox(events.TypeAuditGdprCascadeCompleteV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		done := outbox(events.TypeAuditGdprCascadeCompleteV1)[0]
		assert.Equal(t, pseudonym, done["pseudonym"])
		assert.ElementsMatch(t, []any{"yc-preference", "yc-review"}, done["services"])

		var e api.RegisterEntry
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/gdpr/register/"+user, &e))
		assert.Equal(t, api.RegisterStatus("complete"), e.Status)
		require.NotNil(t, e.Reports)
		assert.Len(t, *e.Reports, 2)
		assert.Equal(t, http.StatusForbidden, shopA.do("/sysadmin/gdpr/register/"+user, nil))
		assert.Equal(t, http.StatusNotFound, sysadmin.do("/sysadmin/gdpr/register/"+uuid.NewString(), nil))

		report("yc-review")
		require.Eventually(t, func() bool { return recorded(events.TypeAuditGdprCascadeCompleteV1) == 1 },
			10*time.Second, 50*time.Millisecond, "the completion is audited too")
		assert.Len(t, outbox(events.TypeAuditGdprCascadeCompleteV1), 1, "a late repeat does not complete it again")
	})

	t.Run("a deletion past its deadline is an incident", func(t *testing.T) {
		user, pseudonym := uuid.NewString(), uuid.NewString()
		publish("shop-b", "yc-identity", events.IdentityUserDeletedV1{
			UserID: user, Pseudonym: pseudonym, Role: "customer", RequestedBy: "system_admin", DeletedAt: time.Now().UTC(),
		})
		publish("shop-b", "yc-review", events.PlatformGdprCascadeCompletedV1{
			UserID: user, Pseudonym: pseudonym, Service: "yc-review", Action: "pseudonymized", RecordCount: 1,
			CompletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			var e api.RegisterEntry
			return sysadmin.do("/sysadmin/gdpr/register/"+user, &e) == http.StatusOK && len(e.MissingServices) == 1
		}, 10*time.Second, 50*time.Millisecond)

		n, err := a.Service.SweepIncidents(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "not due yet")
		time.Sleep(2 * time.Second)
		n, err = a.Service.SweepIncidents(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		incidents := outbox(events.TypeAuditGdprCascadeIncidentV1)
		require.Len(t, incidents, 1)
		assert.Equal(t, user, incidents[0]["user_id"])
		assert.Equal(t, []any{"yc-preference"}, incidents[0]["missing_services"])

		var list api.RegisterList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/gdpr/incidents", &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, api.RegisterStatus("incident"), list.Items[0].Status)
		var open api.RegisterList
		require.Equal(t, http.StatusOK, sysadmin.do("/sysadmin/gdpr/register?status=complete", &open))
		assert.Len(t, open.Items, 1)

		n, err = a.Service.SweepIncidents(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "an incident is raised once")
	})
}

func ptr[T any](v T) *T { return &v }
