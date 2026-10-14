package authctx

import (
	"net/http"
	"slices"
	"strings"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Headers set by yc-gateway after token validation. See
// docs/standards/api-conventions.md §9.
const (
	HeaderAuthorization = "Authorization"
	HeaderTenantID      = "X-YC-Tenant-ID"
	HeaderActorID       = "X-YC-Actor-ID"
	HeaderActorType     = "X-YC-Actor-Type"
	HeaderCorrelationID = "X-YC-Correlation-ID"
)

// SetHeaders writes a onto outbound request headers so the next hop sees the
// same tenant, actor and correlation ID.
func SetHeaders(h http.Header, a AuthContext) {
	setOrDelete(h, HeaderTenantID, a.TenantID)
	setOrDelete(h, HeaderActorID, a.ActorID)
	setOrDelete(h, HeaderActorType, string(a.ActorType))
	setOrDelete(h, HeaderCorrelationID, a.CorrelationID)
	if a.Token != "" {
		h.Set(HeaderAuthorization, "Bearer "+a.Token)
	}
}

// StripHeaders removes the identity headers. yc-gateway calls it on every
// inbound request: only the gateway may set them.
func StripHeaders(h http.Header) {
	h.Del(HeaderTenantID)
	h.Del(HeaderActorID)
	h.Del(HeaderActorType)
}

// BearerToken returns the token from an "Authorization: Bearer" header, or
// "".
func BearerToken(h http.Header) string {
	v := h.Get(HeaderAuthorization)
	const prefix = "bearer "
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

// Middleware establishes the AuthContext from the headers yc-gateway sets.
// Backend services mount it on every route; requests without identity
// headers (health checks) pass through unauthenticated.
//
// Services trust these headers because only yc-gateway can reach them; they
// never accept them from the public network.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, _ := FromContext(r.Context())
		a.TenantID = r.Header.Get(HeaderTenantID)
		a.ActorID = r.Header.Get(HeaderActorID)
		a.ActorType = ActorType(r.Header.Get(HeaderActorType))
		a.Token = BearerToken(r.Header)
		if a.CorrelationID == "" {
			a.CorrelationID = r.Header.Get(HeaderCorrelationID)
		}
		if a.ActorType != "" && !a.ActorType.Valid() {
			ycerrors.WriteError(w, r, ycerrors.Unauthorized("Unknown actor type"))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAuth(r.Context(), a)))
	})
}

// Require rejects requests whose actor type is not one of types: 401 when
// there is no actor, 403 when the actor type is not allowed. With no types,
// any authenticated actor passes.
func Require(types ...ActorType) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a, _ := FromContext(r.Context())
			if !a.Authenticated() {
				ycerrors.WriteError(w, r, ycerrors.Unauthorized("Authentication required"))
				return
			}
			if len(types) > 0 && !slices.Contains(types, a.ActorType) {
				ycerrors.WriteError(w, r, ycerrors.Forbidden("Role not permitted").
					WithInternal("actor_type %s, want one of %v", a.ActorType, types))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func setOrDelete(h http.Header, key, value string) {
	if value == "" {
		h.Del(key)
		return
	}
	h.Set(key, value)
}
