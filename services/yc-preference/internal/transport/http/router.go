// Package http is yc-preference's HTTP transport. Routing, parameter
// parsing and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-preference/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
)

// Preferences is the service the handlers call.
type Preferences interface {
	Get(ctx context.Context) (domain.Preferences, error)
	Update(ctx context.Context, patch domain.Patch) (domain.Preferences, error)
	PutChannel(ctx context.Context, c domain.Channel, address string) (domain.MessagingChannel, bool, error)
	DeleteChannel(ctx context.Context, c domain.Channel) error
	GetDefaults(ctx context.Context) (domain.Defaults, error)
	PutDefaults(ctx context.Context, d domain.Defaults) (domain.Defaults, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Preferences, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-preference registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/preferences", Roles: []string{string(authctx.ActorCustomer), string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/preferences", Roles: []string{string(authctx.ActorShopAdmin)}},
	}
}

type handlers struct {
	svc Preferences
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) GetMyPreferences(ctx context.Context, _ api.GetMyPreferencesRequestObject) (api.GetMyPreferencesResponseObject, error) {
	p, err := h.svc.Get(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetMyPreferences200JSONResponse(preferences(p)), nil
}

func (h handlers) UpdateMyPreferences(ctx context.Context, req api.UpdateMyPreferencesRequestObject) (api.UpdateMyPreferencesResponseObject, error) {
	patch := domain.Patch{DisplayCurrency: req.Body.DisplayCurrency, Locale: req.Body.Locale}
	if c := req.Body.PrimaryChannel; c != nil {
		dc := domain.Channel(*c)
		patch.PrimaryChannel = &dc
	}
	p, err := h.svc.Update(ctx, patch)
	if err != nil {
		return nil, err
	}
	return api.UpdateMyPreferences200JSONResponse(preferences(p)), nil
}

func (h handlers) ListMyChannels(ctx context.Context, _ api.ListMyChannelsRequestObject) (api.ListMyChannelsResponseObject, error) {
	p, err := h.svc.Get(ctx)
	if err != nil {
		return nil, err
	}
	return api.ListMyChannels200JSONResponse{Items: channels(p.Channels)}, nil
}

func (h handlers) PutMyChannel(ctx context.Context, req api.PutMyChannelRequestObject) (api.PutMyChannelResponseObject, error) {
	c, created, err := h.svc.PutChannel(ctx, domain.Channel(req.Body.Channel), req.Body.Address)
	if err != nil {
		return nil, err
	}
	if created {
		return api.PutMyChannel201JSONResponse(channel(c)), nil
	}
	return api.PutMyChannel200JSONResponse(channel(c)), nil
}

func (h handlers) DeleteMyChannel(ctx context.Context, req api.DeleteMyChannelRequestObject) (api.DeleteMyChannelResponseObject, error) {
	if err := h.svc.DeleteChannel(ctx, domain.Channel(req.Channel)); err != nil {
		return nil, err
	}
	return api.DeleteMyChannel204Response{}, nil
}

func (h handlers) GetDefaults(ctx context.Context, _ api.GetDefaultsRequestObject) (api.GetDefaultsResponseObject, error) {
	d, err := h.svc.GetDefaults(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetDefaults200JSONResponse(defaults(d)), nil
}

func (h handlers) PutDefaults(ctx context.Context, req api.PutDefaultsRequestObject) (api.PutDefaultsResponseObject, error) {
	d, err := h.svc.PutDefaults(ctx, domain.Defaults{DisplayCurrency: req.Body.DisplayCurrency, Locale: req.Body.Locale})
	if err != nil {
		return nil, err
	}
	return api.PutDefaults200JSONResponse(defaults(d)), nil
}

func preferences(p domain.Preferences) api.Preferences {
	id, _ := uuid.Parse(p.UserID)
	out := api.Preferences{
		UserID: id, DisplayCurrency: p.DisplayCurrency, Locale: p.Locale,
		Channels: channels(p.Channels), Stored: p.Stored,
	}
	if p.PrimaryChannel != "" {
		c := api.Channel(p.PrimaryChannel)
		out.PrimaryChannel = &c
	}
	if !p.UpdatedAt.IsZero() {
		t := p.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	return out
}

func channels(cs []domain.MessagingChannel) []api.MessagingChannel {
	out := make([]api.MessagingChannel, len(cs))
	for i, c := range cs {
		out[i] = channel(c)
	}
	return out
}

func channel(c domain.MessagingChannel) api.MessagingChannel {
	return api.MessagingChannel{
		Channel: api.Channel(c.Channel), Address: c.Address, Verified: c.Verified,
		VerifiedAt: c.VerifiedAt, CreatedAt: c.CreatedAt.UTC(),
	}
}

func defaults(d domain.Defaults) api.Defaults {
	out := api.Defaults{DisplayCurrency: d.DisplayCurrency, Locale: d.Locale}
	if !d.UpdatedAt.IsZero() {
		t := d.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	return out
}
