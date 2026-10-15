// Package http is yc-review's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-review/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/service"
)

// Reviews is the service the handlers call.
type Reviews interface {
	Submit(ctx context.Context, productID string, c domain.Content) (domain.Review, error)
	Edit(ctx context.Context, id string, c domain.Content) (domain.Review, error)
	Delete(ctx context.Context, id string) error
	ProductReviews(ctx context.Context, productID string, after *repository.Cursor, limit int) (service.Page[domain.Review], error)
	MyReviews(ctx context.Context, productID string, after *repository.Cursor, limit int) (service.Page[domain.Review], error)
	ProductStats(ctx context.Context, productID string) (service.ProductSummary, error)
	Like(ctx context.Context, productID string) (domain.Stats, error)
	Unlike(ctx context.Context, productID string) (domain.Stats, error)
	MyLikes(ctx context.Context, after *repository.Cursor, limit int) (service.Page[repository.Like], error)
	AdminReviews(ctx context.Context, status domain.Status, productID string, after *repository.Cursor, limit int) (service.Page[domain.Review], error)
	AdminReview(ctx context.Context, id string) (domain.Review, error)
	Approve(ctx context.Context, id string) (domain.Review, error)
	Reject(ctx context.Context, id, reason string) (domain.Review, error)
	Respond(ctx context.Context, id, text string) (domain.Review, error)
	Settings(ctx context.Context) (domain.Settings, error)
	PutSettings(ctx context.Context, set domain.Settings) (domain.Settings, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Reviews, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
			httpkit.Idempotency(httpkit.IdempotencyOptions{Store: idem}),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-review registers (ADR-020 rule 7).
// Anyone in the shop may read reviews; the service requires a customer to
// write them.
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/reviews"},
		{Prefix: "/likes", Roles: []string{string(authctx.ActorCustomer)}},
		{Prefix: "/admin/reviews", Roles: admin},
		{Prefix: "/admin/review-settings", Roles: admin},
	}
}

type handlers struct {
	svc Reviews
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListProductReviews(ctx context.Context, req api.ListProductReviewsRequestObject) (api.ListProductReviewsResponseObject, error) {
	size, after, err := page(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ProductReviews(ctx, req.Params.ProductID.String(), after, size)
	if err != nil {
		return nil, err
	}
	return api.ListProductReviews200JSONResponse(reviewList(p, size, req.Params.Cursor, false)), nil
}

func (h handlers) SubmitReview(ctx context.Context, req api.SubmitReviewRequestObject) (api.SubmitReviewResponseObject, error) {
	b := req.Body
	r, err := h.svc.Submit(ctx, b.ProductID.String(), domain.Content{Rating: b.Rating, Title: deref(b.Title), Body: deref(b.Body)})
	if err != nil {
		return nil, err
	}
	return api.SubmitReview201JSONResponse(review(r, true)), nil
}

func (h handlers) ListMyReviews(ctx context.Context, req api.ListMyReviewsRequestObject) (api.ListMyReviewsResponseObject, error) {
	size, after, err := page(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var productID string
	if req.Params.ProductID != nil {
		productID = req.Params.ProductID.String()
	}
	p, err := h.svc.MyReviews(ctx, productID, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListMyReviews200JSONResponse(reviewList(p, size, req.Params.Cursor, true)), nil
}

func (h handlers) GetReviewStats(ctx context.Context, req api.GetReviewStatsRequestObject) (api.GetReviewStatsResponseObject, error) {
	sum, err := h.svc.ProductStats(ctx, req.Params.ProductID.String())
	if err != nil {
		return nil, err
	}
	return api.GetReviewStats200JSONResponse{
		ProductID: req.Params.ProductID, RatingCount: sum.RatingCount, RatingAverage: sum.Average(),
		LikeCount: sum.LikeCount, Distribution: sum.Distribution[:],
	}, nil
}

func (h handlers) EditReview(ctx context.Context, req api.EditReviewRequestObject) (api.EditReviewResponseObject, error) {
	b := req.Body
	r, err := h.svc.Edit(ctx, req.ID.String(), domain.Content{Rating: b.Rating, Title: deref(b.Title), Body: deref(b.Body)})
	if err != nil {
		return nil, err
	}
	return api.EditReview200JSONResponse(review(r, true)), nil
}

func (h handlers) DeleteReview(ctx context.Context, req api.DeleteReviewRequestObject) (api.DeleteReviewResponseObject, error) {
	if err := h.svc.Delete(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteReview204Response{}, nil
}

func (h handlers) ListMyLikes(ctx context.Context, req api.ListMyLikesRequestObject) (api.ListMyLikesResponseObject, error) {
	size, after, err := page(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.MyLikes(ctx, after, size)
	if err != nil {
		return nil, err
	}
	out := api.ListMyLikes200JSONResponse{Items: make([]api.Like, len(p.Items)), Page: pageInfo(size, req.Params.Cursor, p.Next)}
	for i, l := range p.Items {
		out.Items[i] = api.Like{ProductID: mustUUID(l.ProductID), LikedAt: l.LikedAt.UTC()}
	}
	return out, nil
}

func (h handlers) LikeProduct(ctx context.Context, req api.LikeProductRequestObject) (api.LikeProductResponseObject, error) {
	st, err := h.svc.Like(ctx, req.ProductID.String())
	if err != nil {
		return nil, err
	}
	return api.LikeProduct200JSONResponse(stats(req.ProductID, st)), nil
}

func (h handlers) UnlikeProduct(ctx context.Context, req api.UnlikeProductRequestObject) (api.UnlikeProductResponseObject, error) {
	st, err := h.svc.Unlike(ctx, req.ProductID.String())
	if err != nil {
		return nil, err
	}
	return api.UnlikeProduct200JSONResponse(stats(req.ProductID, st)), nil
}

func (h handlers) AdminListReviews(ctx context.Context, req api.AdminListReviewsRequestObject) (api.AdminListReviewsResponseObject, error) {
	size, after, err := page(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var status domain.Status
	if req.Params.Status != nil {
		status = domain.Status(*req.Params.Status)
	}
	var productID string
	if req.Params.ProductID != nil {
		productID = req.Params.ProductID.String()
	}
	p, err := h.svc.AdminReviews(ctx, status, productID, after, size)
	if err != nil {
		return nil, err
	}
	return api.AdminListReviews200JSONResponse(reviewList(p, size, req.Params.Cursor, true)), nil
}

func (h handlers) AdminGetReview(ctx context.Context, req api.AdminGetReviewRequestObject) (api.AdminGetReviewResponseObject, error) {
	r, err := h.svc.AdminReview(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AdminGetReview200JSONResponse(review(r, true)), nil
}

func (h handlers) ApproveReview(ctx context.Context, req api.ApproveReviewRequestObject) (api.ApproveReviewResponseObject, error) {
	r, err := h.svc.Approve(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.ApproveReview200JSONResponse(review(r, true)), nil
}

func (h handlers) RejectReview(ctx context.Context, req api.RejectReviewRequestObject) (api.RejectReviewResponseObject, error) {
	r, err := h.svc.Reject(ctx, req.ID.String(), req.Body.Reason)
	if err != nil {
		return nil, err
	}
	return api.RejectReview200JSONResponse(review(r, true)), nil
}

func (h handlers) RespondToReview(ctx context.Context, req api.RespondToReviewRequestObject) (api.RespondToReviewResponseObject, error) {
	r, err := h.svc.Respond(ctx, req.ID.String(), req.Body.Response)
	if err != nil {
		return nil, err
	}
	return api.RespondToReview200JSONResponse(review(r, true)), nil
}

func (h handlers) GetReviewSettings(ctx context.Context, _ api.GetReviewSettingsRequestObject) (api.GetReviewSettingsResponseObject, error) {
	set, err := h.svc.Settings(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetReviewSettings200JSONResponse(settings(set)), nil
}

func (h handlers) PutReviewSettings(ctx context.Context, req api.PutReviewSettingsRequestObject) (api.PutReviewSettingsResponseObject, error) {
	b := req.Body
	set, err := h.svc.PutSettings(ctx, domain.Settings{
		ModerationMode: domain.ModerationMode(b.ModerationMode), RequirePurchase: b.RequirePurchase, AllowEdit: b.AllowEdit,
	})
	if err != nil {
		return nil, err
	}
	return api.PutReviewSettings200JSONResponse(settings(set)), nil
}

// page parses the page size and cursor.
func page(size *int, cursor *string) (int, *repository.Cursor, error) {
	p, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return 0, nil, err
	}
	if p.Cursor == "" {
		return p.Size, nil, nil
	}
	after := &repository.Cursor{}
	if err := httpkit.DecodeCursor(p.Cursor, after); err != nil {
		return 0, nil, err
	}
	return p.Size, after, nil
}

func pageInfo(size int, cursor *string, next *repository.Cursor) httpkit.PageInfo {
	info := httpkit.PageInfo{Size: size, Cursor: deref(cursor)}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	return info
}

func reviewList(p service.Page[domain.Review], size int, cursor *string, withAuthor bool) api.ReviewList {
	out := api.ReviewList{Items: make([]api.Review, len(p.Items)), Page: pageInfo(size, cursor, p.Next)}
	for i, r := range p.Items {
		out.Items[i] = review(r, withAuthor)
	}
	return out
}

// review is r's API form. A product's public list leaves out the author.
func review(r domain.Review, withAuthor bool) api.Review {
	out := api.Review{
		ReviewID: mustUUID(r.ID), ProductID: mustUUID(r.ProductID), Rating: r.Rating,
		Title: optional(r.Title), Body: optional(r.Body), Status: api.ReviewStatus(r.Status),
		RejectionReason: optional(r.RejectionReason), VerifiedPurchase: r.VerifiedPurchase,
		PurchasePending: r.AwaitingPurchase(), ShopResponse: optional(r.ShopResponse),
		ShopResponseAt: utc(r.ShopResponseAt), ApprovedAt: utc(r.ApprovedAt),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if withAuthor {
		out.UserID = optional(r.UserID)
	}
	return out
}

func stats(productID uuid.UUID, st domain.Stats) api.ProductStats {
	return api.ProductStats{ProductID: productID, RatingCount: st.RatingCount, RatingAverage: st.Average(), LikeCount: st.LikeCount}
}

func settings(set domain.Settings) api.ReviewSettings {
	out := api.ReviewSettings{
		ModerationMode: api.ModerationMode(set.ModerationMode), RequirePurchase: set.RequirePurchase, AllowEdit: set.AllowEdit,
	}
	if !set.UpdatedAt.IsZero() {
		out.UpdatedAt = utc(&set.UpdatedAt)
	}
	return out
}

func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
