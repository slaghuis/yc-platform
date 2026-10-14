package authctx

import (
	"context"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// ActorType is the kind of caller. Values match the actor schema
// (schemas/yc-event-schemas/common/actor.v1.json).
type ActorType string

// Actor types.
const (
	ActorVisitor     ActorType = "visitor"
	ActorCustomer    ActorType = "customer"
	ActorShopAdmin   ActorType = "shop_admin"
	ActorSystemAdmin ActorType = "system_admin"
	ActorService     ActorType = "service"
)

// Valid reports whether t is a known actor type.
func (t ActorType) Valid() bool {
	switch t {
	case ActorVisitor, ActorCustomer, ActorShopAdmin, ActorSystemAdmin, ActorService:
		return true
	default:
		return false
	}
}

// AuthContext is the authenticated caller of the current request or event.
type AuthContext struct {
	TenantID       string
	ActorID        string
	ActorType      ActorType
	CorrelationID  string
	AccessTokenJTI string

	// Token is the raw access token, propagated on outbound calls so the
	// auth chain is preserved through yc-gateway. Never log it.
	Token string
}

// Authenticated reports whether an actor has been established.
func (a AuthContext) Authenticated() bool {
	return a.ActorID != "" && a.ActorType != ""
}

type ctxKey struct{}

// WithAuth attaches a to ctx. The tenant, actor and correlation ID are
// mirrored into yc-telemetry so every log line and span carries them.
func WithAuth(ctx context.Context, a AuthContext) context.Context {
	ctx = context.WithValue(ctx, ctxKey{}, a)
	return telemetry.ContextWithAuthAttrs(ctx, telemetry.AuthAttrs{
		TenantID:       a.TenantID,
		ActorID:        a.ActorID,
		ActorType:      string(a.ActorType),
		CorrelationID:  a.CorrelationID,
		AccessTokenJTI: a.AccessTokenJTI,
	})
}

// FromContext returns the auth context and whether one is present. The
// value may carry only a correlation ID; check Authenticated for an actor.
func FromContext(ctx context.Context) (AuthContext, bool) {
	a, ok := ctx.Value(ctxKey{}).(AuthContext)
	return a, ok
}

// WithCorrelationID sets the correlation ID, keeping any auth already on
// ctx.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	a, _ := FromContext(ctx)
	a.CorrelationID = id
	return WithAuth(ctx, a)
}

// CorrelationID returns the correlation ID on ctx, or "".
func CorrelationID(ctx context.Context) string {
	a, _ := FromContext(ctx)
	return a.CorrelationID
}
