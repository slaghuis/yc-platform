// Package http is yc-identity's HTTP transport.
package http

import (
	"context"
	"net/http"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Identity is the service the handlers call.
type Identity interface {
	CreateVisitor(ctx context.Context, tenantID string) (domain.Session, error)
	ValidateToken(ctx context.Context, raw string) (domain.Session, error)
	Me(ctx context.Context) (domain.User, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Identity, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	h := handlers{svc: svc}
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:      build,
		Checks:     checks,
		Middleware: []func(http.Handler) http.Handler{authctx.Middleware},
	})
	r.Post("/visitor", h.createVisitor)
	r.With(authctx.Require()).Get("/me", h.me)
	// Reached only from inside the platform network; yc-gateway never
	// routes /internal/ paths. Service tokens come with ADR-019's
	// internal-call client.
	r.Post("/internal/validate-token", h.validateToken)
	return r
}

type handlers struct {
	svc Identity
}

// SessionResponse is the body of POST /visitor.
type SessionResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	UserID      string    `json:"user_id"`
	TenantID    string    `json:"tenant_id"`
}

// createVisitor issues a visitor token for the tenant yc-gateway resolved.
func (h handlers) createVisitor(w http.ResponseWriter, r *http.Request) {
	a, _ := authctx.FromContext(r.Context())
	sess, err := h.svc.CreateVisitor(r.Context(), a.TenantID)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, http.StatusCreated, SessionResponse{
		AccessToken: sess.Token,
		TokenType:   "Bearer",
		ExpiresAt:   sess.ExpiresAt.UTC(),
		UserID:      sess.UserID,
		TenantID:    sess.TenantID,
	})
}

// ValidateRequest is the body of POST /internal/validate-token.
type ValidateRequest struct {
	Token string `json:"token"`
}

// ValidateResponse describes a valid token.
type ValidateResponse struct {
	TenantID  string    `json:"tenant_id"`
	ActorID   string    `json:"actor_id"`
	ActorType string    `json:"actor_type"`
	TokenID   string    `json:"jti"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h handlers) validateToken(w http.ResponseWriter, r *http.Request) {
	var req ValidateRequest
	if err := httpkit.DecodeJSON(w, r, &req); err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	if req.Token == "" {
		ycerrors.WriteError(w, r, ycerrors.Validation().Field("token", "required", "Token is required").Build())
		return
	}
	sess, err := h.svc.ValidateToken(r.Context(), req.Token)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, http.StatusOK, ValidateResponse{
		TenantID:  sess.TenantID,
		ActorID:   sess.UserID,
		ActorType: string(sess.Role.ActorType()),
		TokenID:   sess.TokenID,
		ExpiresAt: sess.ExpiresAt.UTC(),
	})
}

// MeResponse is the caller's own user.
type MeResponse struct {
	UserID    string    `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (h handlers) me(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.Me(r.Context())
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, http.StatusOK, MeResponse{
		UserID:    u.ID,
		TenantID:  u.TenantID,
		Role:      string(u.Role),
		Status:    string(u.Status),
		CreatedAt: u.CreatedAt.UTC(),
	})
}
