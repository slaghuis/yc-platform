package main

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

func TestParseOpts(t *testing.T) {
	o, err := parseOpts("list", []string{"--tenant", "demo-shop", "--domain", "order", "--seq", "3,4", "--seq", "9"}, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "yc.demo-shop.order.dlq.v1", o.subject())
	assert.Equal(t, sequences{3, 4, 9}, o.seqs)
	assert.Equal(t, 50, o.limit)

	o, err = parseOpts("show", nil, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "yc.*.*.dlq.v1", o.subject())

	_, err = parseOpts("replay", []string{"--tenant", "demo-shop"}, io.Discard)
	require.ErrorContains(t, err, "--seq, --event-id or --all")
	_, err = parseOpts("replay", []string{"--seq", "0"}, io.Discard)
	require.Error(t, err)
	_, err = parseOpts("list", []string{"--domain", "order.*"}, io.Discard)
	require.Error(t, err)
	_, err = parseOpts("list", []string{"--tenant", "Not A Tenant"}, io.Discard)
	require.Error(t, err)
}

func TestMatches(t *testing.T) {
	d := deadLetter{DLQEnvelope: eventbus.DLQEnvelope{
		OriginalEvent: eventbus.Envelope{EventID: "e1", EventType: "order.placed.v1", TenantID: "shop-a"},
		DLQMetadata:   eventbus.DLQMetadata{Service: "yc-message", Consumer: "yc-message-order-placed"},
	}}
	assert.True(t, opts{}.matches(d))
	assert.True(t, opts{service: "yc-message", eventType: "order.placed.v1", eventID: "e1"}.matches(d))
	assert.False(t, opts{consumer: "yc-review-order-placed"}.matches(d))

	assert.True(t, matchSubject("yc.*.order.dlq.v1", "yc.shop-a.order.dlq.v1"))
	assert.False(t, matchSubject("yc.shop-b.*.dlq.v1", "yc.shop-a.order.dlq.v1"))
}

func TestReplaySubject(t *testing.T) {
	d := deadLetter{Subject: "yc.shop-a.order.dlq.v1", DLQEnvelope: eventbus.DLQEnvelope{
		OriginalEvent: eventbus.Envelope{EventType: "order.placed.v1", TenantID: "shop-a"},
	}}
	subject, err := d.replaySubject()
	require.NoError(t, err)
	assert.Equal(t, "yc.shop-a.order.placed.v1", subject)

	d.Subject = "yc._platform.order.dlq.v1"
	_, err = d.replaySubject()
	assert.ErrorContains(t, err, "not replayed", "a tenant mismatch is never replayed")
}

func TestParse(t *testing.T) {
	data := []byte(`{"original_event":{"event_id":"e1","event_type":"order.placed.v1","tenant_id":"shop-a",
		"payload":{"order_id":"o1"}},"dlq_metadata":{"consumer":"c","attempts":3,"failure_reason":"exhausted"}}`)
	d, err := parse("yc.shop-a.order.dlq.v1", 7, time.Unix(0, 0), data)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), d.Sequence)
	assert.Equal(t, 3, d.DLQMetadata.Attempts)
	assert.Contains(t, string(d.original), `"payload":{"order_id":"o1"}`, "the original bytes are kept")
}
