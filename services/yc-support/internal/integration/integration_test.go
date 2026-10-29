package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/integration"
)

type call struct {
	method, path string
	opts         httpkit.CallOptions
	in           any
}

type fakeCaller struct {
	calls []call
	reply func(c call, out any) error
}

func (f *fakeCaller) Call(_ context.Context, method, path string, opts httpkit.CallOptions, in, out any) error {
	c := call{method, path, opts, in}
	f.calls = append(f.calls, c)
	if f.reply != nil {
		return f.reply(c, out)
	}
	return nil
}

func TestMessagesSendByUserID(t *testing.T) {
	f := &fakeCaller{}
	err := integration.NewMessages(f).Send(context.Background(), integration.Send{
		Type: "support_ack", Reference: "support:r1:created:customer", UserID: "u1",
		Context: map[string]any{"reference": "SUP-AAAAAA"},
	})
	require.NoError(t, err)
	require.Len(t, f.calls, 1)
	c := f.calls[0]
	assert.Equal(t, http.MethodPost, c.method)
	assert.Equal(t, "/internal/send", c.path)
	assert.Equal(t, "support:r1:created:customer", c.opts.IdempotencyKey, "retries send once")
	assert.Equal(t, 3, c.opts.Attempts)

	b, err := json.Marshal(c.in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"support_ack","reference":"support:r1:created:customer","user_id":"u1",
		"context":{"reference":"SUP-AAAAAA"}}`, string(b), "no channel or address: yc-message resolves the user's")
}

func TestMessagesSendByAddress(t *testing.T) {
	f := &fakeCaller{}
	err := integration.NewMessages(f).Send(context.Background(), integration.Send{
		Type: "support_new_request", Reference: "r", Channel: "email", Address: "shop@example.com",
	})
	require.NoError(t, err)
	b, err := json.Marshal(f.calls[0].in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"support_new_request","reference":"r","channel":"email","address":"shop@example.com","context":{}}`, string(b))
}

func TestMessagesSendError(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeCaller{reply: func(call, any) error { return boom }}
	err := integration.NewMessages(f).Send(context.Background(), integration.Send{Type: "x", Reference: "r"})
	assert.ErrorIs(t, err, boom)
}

func tenantCaller(email string, calls *int) *fakeCaller {
	return &fakeCaller{reply: func(c call, out any) error {
		*calls++
		return json.Unmarshal([]byte(`{"tenant_id":"shop-a","name":"Shop A","contact_email":"`+email+`"}`), out)
	}}
}

func TestTenantContactEmailIsCached(t *testing.T) {
	var n int
	f := tenantCaller("hello@shop-a.test", &n)
	tn := integration.NewTenant(f, time.Minute)

	for range 3 {
		email, err := tn.ContactEmail(context.Background(), "shop-a")
		require.NoError(t, err)
		assert.Equal(t, "hello@shop-a.test", email)
	}
	assert.Equal(t, 1, n, "one call for a burst")
	assert.Equal(t, http.MethodGet, f.calls[0].method)
	assert.Equal(t, "/internal/tenants/shop-a", f.calls[0].path)

	_, err := tn.ContactEmail(context.Background(), "shop-b")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the cache is per shop")
}

func TestTenantContactEmailWithoutCache(t *testing.T) {
	var n int
	tn := integration.NewTenant(tenantCaller("a@b.co", &n), 0)
	for range 2 {
		_, err := tn.ContactEmail(context.Background(), "shop-a")
		require.NoError(t, err)
	}
	assert.Equal(t, 2, n)
}

func TestTenantContactEmailMissing(t *testing.T) {
	var n int
	tn := integration.NewTenant(tenantCaller("", &n), time.Minute)
	_, err := tn.ContactEmail(context.Background(), "shop-a")
	assert.ErrorIs(t, err, integration.ErrNoContactEmail)
	_, err = tn.ContactEmail(context.Background(), "shop-a")
	assert.ErrorIs(t, err, integration.ErrNoContactEmail)
	assert.Equal(t, 2, n, "a missing email is not cached")
}

func TestTenantContactEmailError(t *testing.T) {
	boom := errors.New("boom")
	tn := integration.NewTenant(&fakeCaller{reply: func(call, any) error { return boom }}, time.Minute)
	_, err := tn.ContactEmail(context.Background(), "shop-a")
	assert.ErrorIs(t, err, boom)
}

func TestTenantEscapesTheID(t *testing.T) {
	var n int
	f := tenantCaller("a@b.co", &n)
	_, err := integration.NewTenant(f, 0).ContactEmail(context.Background(), "a/b")
	require.NoError(t, err)
	assert.Equal(t, "/internal/tenants/a%2Fb", f.calls[0].path)
}
