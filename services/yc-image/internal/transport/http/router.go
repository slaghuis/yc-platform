// Package http is yc-image's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-image/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/service"
)

// multipartOverhead allows for part headers and the owner_ref field on top
// of the file itself.
const multipartOverhead = 64 << 10

// Images is the service the handlers call.
type Images interface {
	Upload(ctx context.Context, data []byte, ownerRef string) (domain.Image, error)
	RequestUpload(ctx context.Context, contentType string, size int64, ownerRef string) (domain.Image, service.UploadURL, error)
	Finalize(ctx context.Context, id string) (domain.Image, error)
	Get(ctx context.Context, id string) (domain.Image, error)
	VariantURL(ctx context.Context, id, variant, format string) (string, error)
	Delete(ctx context.Context, id string) error
}

// NewRouter returns the service's routes on the standard httpkit chain.
// Bodies may be as large as an upload.
func NewRouter(svc Images, maxUpload int64, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(maxUpload + multipartOverhead),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc, maxUpload: maxUpload}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-image registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/images", Methods: []string{http.MethodGet}},
		{Prefix: "/admin/images", Roles: []string{string(authctx.ActorShopAdmin)}},
	}
}

type handlers struct {
	svc       Images
	maxUpload int64
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) UploadImage(ctx context.Context, req api.UploadImageRequestObject) (api.UploadImageResponseObject, error) {
	var (
		data     []byte
		ownerRef string
		seen     bool
	)
	for {
		part, err := req.Body.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ycerrors.Validation().Message("Malformed multipart body").Wrap(err)
		}
		switch part.FormName() {
		case "file":
			seen = true
			if data, err = readPart(part, h.maxUpload+1); err != nil {
				return nil, err
			}
		case "owner_ref":
			b, err := readPart(part, 200)
			if err != nil {
				return nil, err
			}
			ownerRef = string(b)
		}
	}
	if !seen {
		return nil, ycerrors.Validation().Field("file", "required", "A file part is required").Build()
	}
	img, err := h.svc.Upload(ctx, data, ownerRef)
	if err != nil {
		return nil, err
	}
	return api.UploadImage202JSONResponse(image(img)), nil
}

func readPart(p *multipart.Part, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(p, limit))
	if err != nil {
		return nil, ycerrors.Validation().Message("Malformed multipart body").Wrap(err)
	}
	return b, nil
}

func (h handlers) RequestUploadURL(ctx context.Context, req api.RequestUploadURLRequestObject) (api.RequestUploadURLResponseObject, error) {
	img, up, err := h.svc.RequestUpload(ctx, req.Body.ContentType, req.Body.Bytes, deref(req.Body.OwnerRef))
	if err != nil {
		return nil, err
	}
	return api.RequestUploadURL201JSONResponse{
		Image: image(img), UploadURL: up.URL, Method: "PUT",
		Headers: map[string]string{"Content-Type": img.ContentType}, ExpiresAt: up.ExpiresAt,
	}, nil
}

func (h handlers) FinalizeUpload(ctx context.Context, req api.FinalizeUploadRequestObject) (api.FinalizeUploadResponseObject, error) {
	img, err := h.svc.Finalize(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.FinalizeUpload202JSONResponse(image(img)), nil
}

func (h handlers) DeleteImage(ctx context.Context, req api.DeleteImageRequestObject) (api.DeleteImageResponseObject, error) {
	if err := h.svc.Delete(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteImage204Response{}, nil
}

func (h handlers) GetImage(ctx context.Context, req api.GetImageRequestObject) (api.GetImageResponseObject, error) {
	img, err := h.svc.Get(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetImage200JSONResponse(image(img)), nil
}

func (h handlers) GetImageVariant(ctx context.Context, req api.GetImageVariantRequestObject) (api.GetImageVariantResponseObject, error) {
	u, err := h.svc.VariantURL(ctx, req.ID.String(), string(req.Variant), string(deref(req.Params.Format)))
	if err != nil {
		return nil, err
	}
	return api.GetImageVariant302Response{Headers: api.GetImageVariant302ResponseHeaders{Location: u}}, nil
}

func image(img domain.Image) api.Image {
	out := api.Image{
		ImageID: parseUUID(img.ID), Status: api.ImageStatus(img.Status), ContentType: img.ContentType,
		OwnerRef: optional(img.OwnerRef), FailureReason: optional(img.FailureReason), ChecksumSha256: optional(img.Checksum),
		CreatedAt: img.CreatedAt, ProcessedAt: img.ProcessedAt, Variants: make([]api.Variant, len(img.Variants)),
	}
	if img.Width > 0 {
		out.Width, out.Height = &img.Width, &img.Height
	}
	if img.Bytes > 0 {
		out.Bytes = &img.Bytes
	}
	for i, v := range img.Variants {
		out.Variants[i] = api.Variant{
			Variant: api.VariantName(v.Variant), Format: api.VariantFormat(v.Format),
			Width: v.Width, Height: v.Height, Bytes: v.Bytes, URL: v.URL,
		}
	}
	return out
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseUUID(s string) openapi_types.UUID {
	id, _ := uuid.Parse(s)
	return id
}
