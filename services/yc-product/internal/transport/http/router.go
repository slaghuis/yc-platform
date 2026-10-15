// Package http is yc-product's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-product/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/repository"
)

// Catalog is the service the handlers call.
type Catalog interface {
	CreateSupplier(ctx context.Context, tenantID string, in domain.SupplierInput) (domain.Supplier, error)
	ListSuppliers(ctx context.Context, tenantID string, after *repository.Cursor, limit int) ([]domain.Supplier, *repository.Cursor, error)
	GetSupplier(ctx context.Context, id string) (domain.Supplier, error)
	UpdateSupplier(ctx context.Context, id string, in domain.SupplierInput) (domain.Supplier, error)
	DeleteSupplier(ctx context.Context, id string) error
	PublicSuppliers(ctx context.Context) ([]domain.Supplier, error)

	CreateProduct(ctx context.Context, in domain.ProductInput) (domain.Product, error)
	UpdateProduct(ctx context.Context, id string, in domain.ProductInput) (domain.Product, error)
	DeleteProduct(ctx context.Context, id string) error
	AddVariant(ctx context.Context, productID string, in domain.VariantInput) (domain.Variant, error)
	CreateSKU(ctx context.Context, productID string, in domain.SKUInput) (domain.SKU, error)
	UpdateSKU(ctx context.Context, id string, in domain.SKUInput) (domain.SKU, error)
	DeleteSKU(ctx context.Context, id string) error
	AttachImage(ctx context.Context, productID string, img domain.Image) (domain.Image, error)
	DetachImage(ctx context.Context, productID, imageID string) error
	GetProduct(ctx context.Context, id string) (domain.Product, error)
	GetSKU(ctx context.Context, id string) (domain.SKU, error)
	Browse(ctx context.Context, f domain.BrowseFilter, after *repository.Cursor, limit int) (domain.Browse, *repository.Cursor, error)
	SKUDetails(ctx context.Context, ids []string) ([]domain.SKUDetail, []string, error)

	CreateCategory(ctx context.Context, in domain.CategoryInput) (domain.Category, error)
	UpdateCategory(ctx context.Context, id string, in domain.CategoryInput) (domain.Category, error)
	DeleteCategory(ctx context.Context, id string) error
	CategoryTree(ctx context.Context) ([]*domain.CategoryNode, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Catalog, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-product registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	get := []string{http.MethodGet}
	return []httpkit.RegisteredRoute{
		{Prefix: "/products", Methods: get},
		{Prefix: "/skus", Methods: get},
		{Prefix: "/categories", Methods: get},
		{Prefix: "/suppliers", Methods: get},
		{Prefix: "/admin/products", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/skus", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/categories", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/sysadmin/suppliers", Roles: []string{string(authctx.ActorSystemAdmin)}},
		{Prefix: "/internal/skus", Methods: get},
	}
}

func (h handlers) GetSKUsBatch(ctx context.Context, req api.GetSKUsBatchRequestObject) (api.GetSKUsBatchResponseObject, error) {
	ids := make([]string, len(req.Params.Ids))
	for i, id := range req.Params.Ids {
		ids[i] = id.String()
	}
	found, missing, err := h.svc.SKUDetails(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := api.GetSKUsBatch200JSONResponse{Skus: make([]api.SKUDetail, len(found)), Missing: uuids(missing)}
	for i, d := range found {
		status := api.SKUDetailStatusInactive
		switch {
		case d.Active:
			status = api.SKUDetailStatusActive
		case d.Status == domain.StatusDeleted:
			status = api.SKUDetailStatusDeleted
		}
		out.Skus[i] = api.SKUDetail{
			SkuID: parseUUID(d.ID), ProductID: parseUUID(d.ProductID), SkuCode: d.Code, ProductName: d.ProductName,
			VariantValues: d.VariantValues, IsDigital: d.IsDigital, InventoryTracked: d.InventoryTracked,
			TaxClass: d.TaxClass, WeightGrams: d.WeightGrams, DimensionsMm: dimensions(d.Dimensions), Status: status,
		}
	}
	return out, nil
}

type handlers struct {
	svc Catalog
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) CreateSupplier(ctx context.Context, req api.CreateSupplierRequestObject) (api.CreateSupplierResponseObject, error) {
	in := domain.SupplierInput{Name: req.Body.Name, Contact: deref(req.Body.Contact), Status: status(req.Body.Status)}
	s, err := h.svc.CreateSupplier(ctx, req.Body.TenantID, in)
	if err != nil {
		return nil, err
	}
	return api.CreateSupplier201JSONResponse(supplier(s)), nil
}

func (h handlers) ListSuppliers(ctx context.Context, req api.ListSuppliersRequestObject) (api.ListSuppliersResponseObject, error) {
	page, after, err := parsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	sups, next, err := h.svc.ListSuppliers(ctx, deref(req.Params.TenantID), after, page.Size)
	if err != nil {
		return nil, err
	}
	out := api.ListSuppliers200JSONResponse{Items: make([]api.Supplier, len(sups)), Page: pageInfo(page, next)}
	for i, s := range sups {
		out.Items[i] = supplier(s)
	}
	return out, nil
}

func (h handlers) GetSupplier(ctx context.Context, req api.GetSupplierRequestObject) (api.GetSupplierResponseObject, error) {
	s, err := h.svc.GetSupplier(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetSupplier200JSONResponse(supplier(s)), nil
}

func (h handlers) UpdateSupplier(ctx context.Context, req api.UpdateSupplierRequestObject) (api.UpdateSupplierResponseObject, error) {
	in := domain.SupplierInput{Name: req.Body.Name, Contact: deref(req.Body.Contact), Status: domain.Status(req.Body.Status)}
	s, err := h.svc.UpdateSupplier(ctx, req.ID.String(), in)
	if err != nil {
		return nil, err
	}
	return api.UpdateSupplier200JSONResponse(supplier(s)), nil
}

func (h handlers) DeleteSupplier(ctx context.Context, req api.DeleteSupplierRequestObject) (api.DeleteSupplierResponseObject, error) {
	if err := h.svc.DeleteSupplier(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteSupplier204Response{}, nil
}

func (h handlers) ListPublicSuppliers(ctx context.Context, _ api.ListPublicSuppliersRequestObject) (api.ListPublicSuppliersResponseObject, error) {
	sups, err := h.svc.PublicSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListPublicSuppliers200JSONResponse{Items: make([]api.SupplierSummary, len(sups))}
	for i, s := range sups {
		out.Items[i] = api.SupplierSummary{SupplierID: parseUUID(s.ID), Name: s.Name}
	}
	return out, nil
}

func (h handlers) CreateProduct(ctx context.Context, req api.CreateProductRequestObject) (api.CreateProductResponseObject, error) {
	p, err := h.svc.CreateProduct(ctx, productInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateProduct201JSONResponse(product(p)), nil
}

func (h handlers) UpdateProduct(ctx context.Context, req api.UpdateProductRequestObject) (api.UpdateProductResponseObject, error) {
	p, err := h.svc.UpdateProduct(ctx, req.ID.String(), productInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateProduct200JSONResponse(product(p)), nil
}

func (h handlers) DeleteProduct(ctx context.Context, req api.DeleteProductRequestObject) (api.DeleteProductResponseObject, error) {
	if err := h.svc.DeleteProduct(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteProduct204Response{}, nil
}

func (h handlers) AddVariant(ctx context.Context, req api.AddVariantRequestObject) (api.AddVariantResponseObject, error) {
	va, err := h.svc.AddVariant(ctx, req.ID.String(), domain.VariantInput{AxisName: req.Body.AxisName, Values: req.Body.Values})
	if err != nil {
		return nil, err
	}
	return api.AddVariant201JSONResponse(variant(va)), nil
}

func (h handlers) CreateSKU(ctx context.Context, req api.CreateSKURequestObject) (api.CreateSKUResponseObject, error) {
	s, err := h.svc.CreateSKU(ctx, req.ID.String(), skuInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateSKU201JSONResponse(sku(s)), nil
}

func (h handlers) UpdateSKU(ctx context.Context, req api.UpdateSKURequestObject) (api.UpdateSKUResponseObject, error) {
	s, err := h.svc.UpdateSKU(ctx, req.ID.String(), skuInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateSKU200JSONResponse(sku(s)), nil
}

func (h handlers) DeleteSKU(ctx context.Context, req api.DeleteSKURequestObject) (api.DeleteSKUResponseObject, error) {
	if err := h.svc.DeleteSKU(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteSKU204Response{}, nil
}

func (h handlers) AttachImage(ctx context.Context, req api.AttachImageRequestObject) (api.AttachImageResponseObject, error) {
	img, err := h.svc.AttachImage(ctx, req.ID.String(), domain.Image{
		ImageID: req.Body.ImageID.String(), SortOrder: deref(req.Body.SortOrder), IsPrimary: deref(req.Body.IsPrimary),
	})
	if err != nil {
		return nil, err
	}
	return api.AttachImage201JSONResponse(image(img)), nil
}

func (h handlers) DetachImage(ctx context.Context, req api.DetachImageRequestObject) (api.DetachImageResponseObject, error) {
	if err := h.svc.DetachImage(ctx, req.ID.String(), req.ImageID.String()); err != nil {
		return nil, err
	}
	return api.DetachImage204Response{}, nil
}

func (h handlers) GetProduct(ctx context.Context, req api.GetProductRequestObject) (api.GetProductResponseObject, error) {
	p, err := h.svc.GetProduct(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetProduct200JSONResponse(product(p)), nil
}

func (h handlers) GetSKU(ctx context.Context, req api.GetSKURequestObject) (api.GetSKUResponseObject, error) {
	s, err := h.svc.GetSKU(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetSKU200JSONResponse(sku(s)), nil
}

func (h handlers) BrowseProducts(ctx context.Context, req api.BrowseProductsRequestObject) (api.BrowseProductsResponseObject, error) {
	p := req.Params
	page, after, err := parsePage(p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	f := domain.BrowseFilter{Query: deref(p.Q)}
	if p.Category != nil {
		f.CategoryID = p.Category.String()
	}
	if p.SupplierID != nil {
		f.SupplierID = p.SupplierID.String()
	}
	if p.Sort != nil {
		f.Sort = domain.Sort(*p.Sort)
	}
	f.InStock = deref(p.InStock)
	if deref(p.Favourites) {
		a, _ := authctx.FromContext(ctx)
		f.FavouritesOf = a.ActorID
	}
	res, next, err := h.svc.Browse(ctx, f, after, page.Size)
	if err != nil {
		return nil, err
	}
	out := api.BrowseProducts200JSONResponse{
		Items: make([]api.ProductSummary, len(res.Items)), Sort: api.Sort(res.Sort), Page: pageInfo(page, next),
	}
	for i, it := range res.Items {
		out.Items[i] = summary(it)
	}
	if fav := res.Favourites; fav != nil {
		out.Facets.Favourites = &struct {
			ProductCount int `json:"product_count"`
		}{ProductCount: fav.ProductCount}
	}
	return out, nil
}

func (h handlers) CreateCategory(ctx context.Context, req api.CreateCategoryRequestObject) (api.CreateCategoryResponseObject, error) {
	c, err := h.svc.CreateCategory(ctx, categoryInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateCategory201JSONResponse(category(c)), nil
}

func (h handlers) UpdateCategory(ctx context.Context, req api.UpdateCategoryRequestObject) (api.UpdateCategoryResponseObject, error) {
	c, err := h.svc.UpdateCategory(ctx, req.ID.String(), categoryInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateCategory200JSONResponse(category(c)), nil
}

func (h handlers) DeleteCategory(ctx context.Context, req api.DeleteCategoryRequestObject) (api.DeleteCategoryResponseObject, error) {
	if err := h.svc.DeleteCategory(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteCategory204Response{}, nil
}

func (h handlers) GetCategoryTree(ctx context.Context, _ api.GetCategoryTreeRequestObject) (api.GetCategoryTreeResponseObject, error) {
	roots, err := h.svc.CategoryTree(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetCategoryTree200JSONResponse{Items: categoryNodes(roots)}, nil
}

func parsePage(size *int, cursor *string) (httpkit.PageRequest, *repository.Cursor, error) {
	page, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return page, nil, err
	}
	if page.Cursor == "" {
		return page, nil, nil
	}
	after := &repository.Cursor{}
	if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
		return page, nil, err
	}
	return page, after, nil
}

func pageInfo(page httpkit.PageRequest, next *repository.Cursor) httpkit.PageInfo {
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	return info
}

func productInput(b *api.ProductInput) domain.ProductInput {
	in := domain.ProductInput{
		Name:        b.Name,
		Description: deref(b.Description),
		IsDigital:   deref(b.IsDigital),
		Status:      status(b.Status),
		Attributes:  deref(b.Attributes),
	}
	if b.SupplierID != nil {
		in.SupplierID = b.SupplierID.String()
	}
	if b.CategoryIds != nil {
		in.CategoryIDs = uuidStrings(*b.CategoryIds)
	}
	return in
}

func skuInput(b *api.SKUInput) domain.SKUInput {
	in := domain.SKUInput{
		Code:             b.SkuCode,
		VariantValues:    deref(b.VariantValues),
		InventoryTracked: true,
		TaxClass:         b.TaxClass,
		WeightGrams:      b.WeightGrams,
		Status:           status(b.Status),
	}
	if b.InventoryTracked != nil {
		in.InventoryTracked = *b.InventoryTracked
	}
	if d := b.DimensionsMm; d != nil {
		in.Dimensions = &domain.Dimensions{LengthMM: d.Length, WidthMM: d.Width, HeightMM: d.Height}
	}
	return in
}

func dimensions(d *domain.Dimensions) *api.Dimensions {
	if d == nil {
		return nil
	}
	return &api.Dimensions{Length: d.LengthMM, Width: d.WidthMM, Height: d.HeightMM}
}

func categoryInput(b *api.CategoryInput) domain.CategoryInput {
	in := domain.CategoryInput{Name: b.Name, Slug: b.Slug, SortOrder: deref(b.SortOrder)}
	if b.ParentID != nil {
		in.ParentID = b.ParentID.String()
	}
	return in
}

func supplier(s domain.Supplier) api.Supplier {
	return api.Supplier{
		SupplierID: parseUUID(s.ID), TenantID: s.TenantID, Name: s.Name, Contact: s.Contact,
		Status: api.Status(s.Status), CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
	}
}

func product(p domain.Product) api.Product {
	out := api.Product{
		ProductID: parseUUID(p.ID), SupplierID: optionalUUID(p.SupplierID), Name: p.Name,
		Description: p.Description, IsDigital: p.IsDigital, Status: api.Status(p.Status),
		Attributes: p.Attributes, CategoryIds: uuids(p.CategoryIDs),
		Variants: make([]api.Variant, len(p.Variants)), Skus: make([]api.SKU, len(p.SKUs)),
		Images: make([]api.ProductImage, len(p.Images)), CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
	for i, va := range p.Variants {
		out.Variants[i] = variant(va)
	}
	for i, s := range p.SKUs {
		out.Skus[i] = sku(s)
	}
	for i, img := range p.Images {
		out.Images[i] = image(img)
	}
	return out
}

func summary(p domain.ProductSummary) api.ProductSummary {
	out := api.ProductSummary{
		ProductID: parseUUID(p.ID), SupplierID: optionalUUID(p.SupplierID), Name: p.Name,
		Description: p.Description, IsDigital: p.IsDigital, InStock: p.InStock, CategoryIds: uuids(p.CategoryIDs),
		PrimaryImageID: optionalUUID(p.PrimaryImageID), Skus: make([]api.SKUSummary, len(p.SKUs)),
		Reviews: api.ReviewStats{
			RatingCount: int(p.Reviews.RatingCount), RatingAverage: p.Reviews.Average(), LikeCount: int(p.Reviews.LikeCount),
		},
	}
	for i, s := range p.SKUs {
		out.Skus[i] = api.SKUSummary{SkuID: parseUUID(s.ID), SkuCode: s.Code, VariantValues: s.VariantValues}
	}
	return out
}

func variant(va domain.Variant) api.Variant {
	return api.Variant{VariantID: parseUUID(va.ID), AxisName: va.AxisName, Values: va.Values, CreatedAt: va.CreatedAt.UTC()}
}

func sku(s domain.SKU) api.SKU {
	return api.SKU{
		SkuID: parseUUID(s.ID), ProductID: parseUUID(s.ProductID), SkuCode: s.Code, VariantValues: s.VariantValues,
		InventoryTracked: s.InventoryTracked, TaxClass: s.TaxClass, WeightGrams: s.WeightGrams,
		DimensionsMm: dimensions(s.Dimensions),
		Status:       api.Status(s.Status), CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
	}
}

func image(img domain.Image) api.ProductImage {
	return api.ProductImage{ImageID: parseUUID(img.ImageID), SortOrder: img.SortOrder, IsPrimary: img.IsPrimary}
}

func category(c domain.Category) api.Category {
	return api.Category{
		CategoryID: parseUUID(c.ID), ParentID: optionalUUID(c.ParentID), Name: c.Name, Slug: c.Slug, SortOrder: c.SortOrder,
	}
}

func categoryNodes(ns []*domain.CategoryNode) []api.CategoryNode {
	out := make([]api.CategoryNode, len(ns))
	for i, n := range ns {
		out[i] = api.CategoryNode{
			CategoryID: parseUUID(n.ID), ParentID: optionalUUID(n.ParentID), Name: n.Name, Slug: n.Slug,
			SortOrder: n.SortOrder, Children: categoryNodes(n.Children),
		}
	}
	return out
}

func status(s *api.Status) domain.Status {
	if s == nil {
		return ""
	}
	return domain.Status(*s)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func uuidStrings(ids []openapi_types.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func uuids(ids []string) []openapi_types.UUID {
	out := make([]openapi_types.UUID, len(ids))
	for i, id := range ids {
		out[i] = parseUUID(id)
	}
	return out
}

func optionalUUID(s string) *openapi_types.UUID {
	if s == "" {
		return nil
	}
	id := parseUUID(s)
	return &id
}

// parseUUID converts IDs the database generated; they are always UUIDs.
func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}
