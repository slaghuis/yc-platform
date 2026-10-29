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
	CreateSupplier(ctx context.Context, in domain.SupplierInput) (domain.Supplier, error)
	ListSuppliers(ctx context.Context, after *repository.Cursor, limit int) ([]domain.Supplier, *repository.Cursor, error)
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
	PutRelated(ctx context.Context, productID string, ids []string) ([]string, error)
	AlsoBought(ctx context.Context, productID string, limit int) ([]domain.ProductSummary, error)
	Browse(ctx context.Context, f domain.BrowseFilter, after *repository.Cursor, limit int) (domain.Browse, *repository.Cursor, error)
	SKUDetails(ctx context.Context, ids []string) ([]domain.SKUDetail, []string, error)
	CreateBundle(ctx context.Context, in domain.BundleInput) (domain.Product, error)
	UpdateBundle(ctx context.Context, productID string, in domain.BundleUpdateInput) (domain.Product, error)
	GetBundle(ctx context.Context, productID string) (domain.Product, error)

	CreateCategory(ctx context.Context, in domain.CategoryInput) (domain.Category, error)
	UpdateCategory(ctx context.Context, id string, in domain.CategoryInput) (domain.Category, error)
	DeleteCategory(ctx context.Context, id string) error
	CategoryTree(ctx context.Context) ([]*domain.CategoryNode, error)

	CreateBanner(ctx context.Context, in domain.BannerInput) (domain.Banner, error)
	UpdateBanner(ctx context.Context, id string, in domain.BannerInput) (domain.Banner, error)
	DeleteBanner(ctx context.Context, id string) error
	ReorderBanners(ctx context.Context, ids []string) error
	Banners(ctx context.Context) ([]domain.Banner, error)
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
		{Prefix: "/banners", Methods: get},
		{Prefix: "/suppliers", Methods: get},
		{Prefix: "/admin/products", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/bundles", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/skus", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/categories", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/banners", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/admin/suppliers", Roles: []string{string(authctx.ActorShopAdmin)}},
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
			VariantValues: d.VariantValues, IsDigital: d.IsDigital, Kind: api.ProductKind(d.Kind),
			InventoryTracked: d.InventoryTracked, TaxClass: d.TaxClass, WeightGrams: d.WeightGrams,
			DimensionsMm: dimensions(d.Dimensions), Status: status, Components: bundleComponents(d.Components),
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
	s, err := h.svc.CreateSupplier(ctx, in)
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
	sups, next, err := h.svc.ListSuppliers(ctx, after, page.Size)
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

func (h handlers) PutRelatedProducts(ctx context.Context, req api.PutRelatedProductsRequestObject) (api.PutRelatedProductsResponseObject, error) {
	ids, err := h.svc.PutRelated(ctx, req.ID.String(), uuidStrings(req.Body.ProductIds))
	if err != nil {
		return nil, err
	}
	return api.PutRelatedProducts200JSONResponse{ProductIds: uuids(ids)}, nil
}

func (h handlers) CreateBundle(ctx context.Context, req api.CreateBundleRequestObject) (api.CreateBundleResponseObject, error) {
	p, err := h.svc.CreateBundle(ctx, bundleInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateBundle201JSONResponse(product(p)), nil
}

func (h handlers) UpdateBundle(ctx context.Context, req api.UpdateBundleRequestObject) (api.UpdateBundleResponseObject, error) {
	p, err := h.svc.UpdateBundle(ctx, req.ID.String(), bundleUpdateInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateBundle200JSONResponse(product(p)), nil
}

func (h handlers) GetBundle(ctx context.Context, req api.GetBundleRequestObject) (api.GetBundleResponseObject, error) {
	p, err := h.svc.GetBundle(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetBundle200JSONResponse(product(p)), nil
}

func (h handlers) GetAlsoBought(ctx context.Context, req api.GetAlsoBoughtRequestObject) (api.GetAlsoBoughtResponseObject, error) {
	limit := domain.DefaultAlsoBought
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	items, err := h.svc.AlsoBought(ctx, req.ID.String(), limit)
	if err != nil {
		return nil, err
	}
	out := api.GetAlsoBought200JSONResponse{Items: make([]api.ProductSummary, len(items))}
	for i, it := range items {
		out.Items[i] = summary(it)
	}
	return out, nil
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
		f.CategoryIDs = uuidStrings(*p.Category)
	}
	if p.ExcludeCategory != nil {
		f.ExcludeCategoryIDs = uuidStrings(*p.ExcludeCategory)
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

func (h handlers) CreateBanner(ctx context.Context, req api.CreateBannerRequestObject) (api.CreateBannerResponseObject, error) {
	b, err := h.svc.CreateBanner(ctx, bannerInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateBanner201JSONResponse(banner(b)), nil
}

func (h handlers) UpdateBanner(ctx context.Context, req api.UpdateBannerRequestObject) (api.UpdateBannerResponseObject, error) {
	b, err := h.svc.UpdateBanner(ctx, req.ID.String(), bannerInput(req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateBanner200JSONResponse(banner(b)), nil
}

func (h handlers) DeleteBanner(ctx context.Context, req api.DeleteBannerRequestObject) (api.DeleteBannerResponseObject, error) {
	if err := h.svc.DeleteBanner(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteBanner204Response{}, nil
}

func (h handlers) ReorderBanners(ctx context.Context, req api.ReorderBannersRequestObject) (api.ReorderBannersResponseObject, error) {
	if err := h.svc.ReorderBanners(ctx, uuidStrings(req.Body.Ids)); err != nil {
		return nil, err
	}
	return api.ReorderBanners204Response{}, nil
}

func (h handlers) ListBanners(ctx context.Context, _ api.ListBannersRequestObject) (api.ListBannersResponseObject, error) {
	banners, err := h.svc.Banners(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]api.Banner, len(banners))
	for i, b := range banners {
		items[i] = banner(b)
	}
	return api.ListBanners200JSONResponse{Items: items}, nil
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
	if b.Kind != nil {
		in.Kind = domain.ProductKind(*b.Kind)
	}
	if b.SupplierID != nil {
		in.SupplierID = b.SupplierID.String()
	}
	if b.CategoryIds != nil {
		in.CategoryIDs = uuidStrings(*b.CategoryIds)
	}
	return in
}

func bundleInput(b *api.BundleInput) domain.BundleInput {
	in := domain.BundleInput{
		Name: b.Name, Description: deref(b.Description), SKUCode: b.SkuCode, TaxClass: b.TaxClass,
		Components: bundleComponentInputs(b.Components),
	}
	if b.CategoryIds != nil {
		in.CategoryIDs = uuidStrings(*b.CategoryIds)
	}
	return in
}

func bundleUpdateInput(b *api.BundleUpdateInput) domain.BundleUpdateInput {
	in := domain.BundleUpdateInput{
		Description: b.Description, Components: bundleComponentInputs(b.Components),
	}
	if b.Name != nil {
		in.Name = *b.Name
	}
	if b.CategoryIds != nil {
		ids := uuidStrings(*b.CategoryIds)
		in.CategoryIDs = &ids
	}
	return in
}

func bundleComponentInputs(in []api.BundleComponentInput) []domain.BundleComponentInput {
	out := make([]domain.BundleComponentInput, len(in))
	for i, c := range in {
		out[i] = domain.BundleComponentInput{SKUID: c.SkuID.String(), Quantity: c.Quantity}
	}
	return out
}

func bundleComponents(in []domain.BundleComponent) []api.BundleComponent {
	out := make([]api.BundleComponent, len(in))
	for i, c := range in {
		out[i] = api.BundleComponent{
			SkuID: parseUUID(c.SKUID), ProductID: parseUUID(c.ProductID),
			ProductName: c.ProductName, Quantity: c.Quantity,
		}
	}
	return out
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

func bannerInput(b *api.BannerInput) domain.BannerInput {
	return domain.BannerInput{
		CategoryID: b.CategoryID.String(), Text: b.Text,
		BackgroundImageID: b.BackgroundImageID.String(), LargeImageID: b.LargeImageID.String(),
	}
}

func banner(b domain.Banner) api.Banner {
	return api.Banner{
		BannerID: parseUUID(b.ID), CategoryID: parseUUID(b.CategoryID), CategoryName: b.CategoryName,
		Text: b.Text, BackgroundImageID: parseUUID(b.BackgroundImageID), LargeImageID: parseUUID(b.LargeImageID),
		SortOrder: b.SortOrder,
	}
}

func categoryInput(b *api.CategoryInput) domain.CategoryInput {
	in := domain.CategoryInput{Name: b.Name, Slug: b.Slug, SortOrder: deref(b.SortOrder)}
	if b.ParentID != nil {
		in.ParentID = b.ParentID.String()
	}
	if b.ImageID != nil {
		in.ImageID = b.ImageID.String()
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
	kind := p.Kind
	if kind == "" {
		kind = domain.KindStandard
	}
	out := api.Product{
		ProductID: parseUUID(p.ID), SupplierID: optionalUUID(p.SupplierID), Name: p.Name,
		Description: p.Description, IsDigital: p.IsDigital, Kind: api.ProductKind(kind), Status: api.Status(p.Status),
		Attributes: p.Attributes, CategoryIds: uuids(p.CategoryIDs), RelatedProductIds: uuids(p.RelatedProductIDs),
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
	if p.Bundle != nil {
		b := api.Bundle{Components: bundleComponents(p.Bundle.Components)}
		out.Bundle = &b
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
		CategoryID: parseUUID(c.ID), ParentID: optionalUUID(c.ParentID), Name: c.Name, Slug: c.Slug,
		SortOrder: c.SortOrder, ImageID: optionalUUID(c.ImageID),
	}
}

func categoryNodes(ns []*domain.CategoryNode) []api.CategoryNode {
	out := make([]api.CategoryNode, len(ns))
	for i, n := range ns {
		out[i] = api.CategoryNode{
			CategoryID: parseUUID(n.ID), ParentID: optionalUUID(n.ParentID), Name: n.Name, Slug: n.Slug,
			SortOrder: n.SortOrder, ImageID: optionalUUID(n.ImageID), Children: categoryNodes(n.Children),
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
