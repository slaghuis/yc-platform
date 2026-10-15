//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

func TestListShowReplay(t *testing.T) {
	ctx := context.Background()
	telemetry.InitForTest(t, "yc-dlq-cli")
	natsURL := testkit.NATS(t)
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-test", Validator: eventbus.NoopValidator{},
		Streams: []eventbus.StreamConfig{eventbus.DLQStream(), eventbus.OrderStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	deadLetter := func(dlqTenant, eventTenant, consumer string) string {
		id := uuid.NewString()
		body, err := json.Marshal(eventbus.DLQEnvelope{
			OriginalEvent: eventbus.Envelope{
				EventID: id, EventType: "order.placed.v1", SchemaVersion: 1, EventTime: time.Now().UTC(),
				TenantID: eventTenant, SourceService: "yc-orders", Payload: json.RawMessage(`{"order_id":"o1"}`),
			},
			DLQMetadata: eventbus.DLQMetadata{
				Service: "yc-message", Consumer: consumer, Attempts: 4, FailureReason: "exhausted",
				LastError: "preference lookup: connection refused",
			},
		})
		require.NoError(t, err)
		require.NoError(t, nc.Publish("yc."+dlqTenant+".order.dlq.v1", body))
		return id
	}
	first := deadLetter("shop-a", "shop-a", "yc-message-order-placed")
	second := deadLetter("shop-a", "shop-a", "yc-review-order-placed")
	mismatched := deadLetter("_platform", "shop-b", "yc-message-order-placed")
	require.NoError(t, nc.Flush())

	cli := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(ctx, append(args, "--nats-url", natsURL), &out, &out)
		return out.String(), err
	}

	require.Eventually(t, func() bool {
		out, err := cli("list")
		return err == nil && bytes.Contains([]byte(out), []byte("3 dead letter(s)"))
	}, 10*time.Second, 100*time.Millisecond)
	out, err := cli("list", "--tenant", "shop-a", "--consumer", "yc-message-order-placed")
	require.NoError(t, err)
	assert.Contains(t, out, first)
	assert.NotContains(t, out, second)
	assert.Contains(t, out, "1 dead letter(s)")

	out, err = cli("show", "--event-id", second)
	require.NoError(t, err)
	assert.Contains(t, out, `"consumer": "yc-review-order-placed"`)

	sub, err := nc.SubscribeSync("yc.shop-a.order.placed.v1")
	require.NoError(t, err)
	out, err = cli("replay", "--event-id", first, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "would replay")
	_, err = sub.NextMsg(300 * time.Millisecond)
	require.ErrorIs(t, err, nats.ErrTimeout, "a dry run publishes nothing")

	out, err = cli("replay", "--event-id", first)
	require.NoError(t, err, out)
	msg, err := sub.NextMsg(5 * time.Second)
	require.NoError(t, err)
	var env eventbus.Envelope
	require.NoError(t, json.Unmarshal(msg.Data, &env))
	assert.Equal(t, first, env.EventID, "the original event, unchanged")
	assert.NotEmpty(t, msg.Header.Get(ReplayHeader))

	out, err = cli("replay", "--event-id", mismatched)
	require.Error(t, err)
	assert.Contains(t, out, "not replayed")
}
