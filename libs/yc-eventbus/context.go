package eventbus

import (
	"context"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

type consumedKey struct{}

// extractTrace continues the publisher's trace from message headers. NATS
// header keys are case-sensitive and the propagator writes lowercase keys,
// so http.Header's canonicalising lookup would miss them.
func extractTrace(parent context.Context, hdr nats.Header) context.Context {
	carrier := propagation.MapCarrier{}
	for k, v := range hdr {
		if len(v) > 0 {
			carrier[strings.ToLower(k)] = v[0]
		}
	}
	return otel.GetTextMapPropagator().Extract(parent, carrier)
}

// withEnvelope makes the envelope's tenant, actor and correlation ID the
// auth context, so handler logs correlate with the request that caused the
// event. Events the handler publishes inherit the correlation ID and take
// the consumed event as their causation.
func withEnvelope(ctx context.Context, env *Envelope) context.Context {
	ctx = context.WithValue(ctx, consumedKey{}, env.EventID)
	return authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID:       env.TenantID,
		ActorID:        env.Actor.ID,
		ActorType:      authctx.ActorType(env.Actor.Type),
		CorrelationID:  env.CorrelationID,
		AccessTokenJTI: env.Actor.AccessTokenJTI,
	})
}

func consumedEventID(ctx context.Context) string {
	id, _ := ctx.Value(consumedKey{}).(string)
	return id
}
