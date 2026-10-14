package eventbus_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

func TestEnvelope_RoundTrip(t *testing.T) {
	t.Parallel()
	payload, _ := json.Marshal(map[string]any{"user_id": "u-1"})
	env := eventbus.Envelope{
		EventID:       "e-1",
		EventType:     "identity.user.registered.v1",
		EventTime:     time.Now().UTC().Truncate(time.Millisecond),
		TenantID:      "shop-abc",
		Actor:         eventbus.Actor{Type: "customer", ID: "u-1"},
		CorrelationID: "c-1",
		SourceService: "yc-identity",
		SchemaVersion: 1,
		Payload:       payload,
	}
	b, err := json.Marshal(env)
	require.NoError(t, err)

	var got eventbus.Envelope
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, env.EventID, got.EventID)
	assert.Equal(t, env.EventType, got.EventType)
	assert.Equal(t, env.TenantID, got.TenantID)
}

func TestDLQEnvelope_Marshal(t *testing.T) {
	t.Parallel()
	dlq := eventbus.DLQEnvelope{
		OriginalEvent: eventbus.Envelope{EventID: "e-1", EventType: "order.placed.v1"},
		DLQMetadata: eventbus.DLQMetadata{
			Consumer:      "yc-message-order-placed",
			Attempts:      5,
			FirstFailure:  time.Now().UTC(),
			LastFailure:   time.Now().UTC(),
			LastError:     "boom",
			FailureReason: "exhausted",
		},
	}
	b, err := json.Marshal(dlq)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"consumer":"yc-message-order-placed"`)
}