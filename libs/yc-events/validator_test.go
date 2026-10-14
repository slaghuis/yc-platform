package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

func envelope(t *testing.T, payload any) *eventbus.Envelope {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return &eventbus.Envelope{
		EventID:       "0192f3a4-5b6c-7d8e-9f01-23456789abcd",
		EventType:     events.TypeIdentityVisitorCreatedV1,
		EventTime:     time.Now().UTC(),
		TenantID:      "shop-a",
		Actor:         eventbus.Actor{Type: "visitor", ID: "0192f3a4-5b6c-7d8e-9f01-23456789abce"},
		CorrelationID: "0192f3a4-5b6c-7d8e-9f01-23456789abcf",
		SourceService: "yc-identity",
		SchemaVersion: 1,
		Payload:       b,
	}
}

func visitorCreated() events.IdentityVisitorCreatedV1 {
	now := time.Now().UTC()
	return events.IdentityVisitorCreatedV1{
		UserID:         "0192f3a4-5b6c-7d8e-9f01-23456789abce",
		CreatedAt:      now,
		TokenExpiresAt: now.Add(time.Hour),
	}
}

func TestValidator(t *testing.T) {
	v, err := events.NewValidator()
	require.NoError(t, err)
	ctx := context.Background()

	t.Run("every event type has a compiled schema", func(t *testing.T) {
		for _, et := range events.EventTypes {
			env := &eventbus.Envelope{EventType: et, Payload: json.RawMessage(`{}`)}
			err := v.ValidatePayload(ctx, env)
			require.Error(t, err, et)
			assert.NotContains(t, err.Error(), "no schema", et)
		}
	})

	t.Run("valid envelope and payload pass", func(t *testing.T) {
		env := envelope(t, visitorCreated())
		assert.NoError(t, v.ValidateEnvelope(ctx, env))
		assert.NoError(t, v.ValidatePayload(ctx, env))
	})

	t.Run("envelope formats are asserted", func(t *testing.T) {
		env := envelope(t, visitorCreated())
		env.CorrelationID = "not-a-uuid"
		assert.ErrorContains(t, v.ValidateEnvelope(ctx, env), "correlation_id")
	})

	t.Run("envelope tenant pattern applies", func(t *testing.T) {
		env := envelope(t, visitorCreated())
		env.TenantID = "Shop_A"
		assert.Error(t, v.ValidateEnvelope(ctx, env))
	})

	t.Run("missing required payload field fails", func(t *testing.T) {
		env := envelope(t, map[string]any{"user_id": "0192f3a4-5b6c-7d8e-9f01-23456789abce"})
		assert.ErrorContains(t, v.ValidatePayload(ctx, env), "created_at")
	})

	t.Run("unknown payload field fails", func(t *testing.T) {
		p := map[string]any{}
		b, _ := json.Marshal(visitorCreated())
		require.NoError(t, json.Unmarshal(b, &p))
		p["email"] = "x@example.com"
		assert.Error(t, v.ValidatePayload(ctx, envelope(t, p)))
	})

	t.Run("unknown event type fails", func(t *testing.T) {
		env := envelope(t, visitorCreated())
		env.EventType = "identity.visitor.vanished.v1"
		assert.ErrorContains(t, v.ValidatePayload(ctx, env), "no schema")
	})

	t.Run("tenant provisioned with optional fields absent passes", func(t *testing.T) {
		env := envelope(t, events.PlatformTenantProvisionedV1{
			TenantID:            "shop-a",
			Name:                "Shop A",
			Hostnames:           []string{"shop-a.localhost"},
			StoreCurrency:       "ZAR",
			DefaultJurisdiction: "ZA",
			IsTaxInclusive:      true,
			ProvisionedAt:       time.Now().UTC(),
		})
		env.EventType = events.TypePlatformTenantProvisionedV1
		assert.NoError(t, v.ValidatePayload(ctx, env))
	})
}
