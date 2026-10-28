//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-product/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorType string) client {
	h := map[string]string{authctx.HeaderTenantID: tenant}
	if actorType != "" {
		h[authctx.HeaderActorID] = "00000000-0000-0000-0000-0000000000a1"
		h[authctx.HeaderActorType] = actorType
	}
	return client{t: t, srv: srv, hdr: h}
}

// withCaller marks the client as a service call through yc-gateway's
// internal listener (ADR-021).
func (c client) withCaller() client {
	c.hdr[authctx.HeaderCallerService] = "yc-checkout"
	return c
}

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, &buf)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func TestProduct(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-product")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	t.Setenv("YC_PRODUCT_DB_URL", dbURL)
	natsURL := testkit.NATS(t)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-product", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	testkit.AssertTenantIsolation(t, db, "outbox")
	eventsOf := func(tenant, eventType string) int {
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2`, tenant, eventType).Scan(&n))
		return n
	}

	sys := as(t, srv, "_platform", "system_admin")
	admin := as(t, srv, "shop-a", "shop_admin")
	visitor := as(t, srv, "shop-a", "visitor")
	otherShop := as(t, srv, "shop-b", "visitor")

	var sup api.Supplier
	var apparel, tshirts api.Category
	var shirt api.Product
	var red, blue api.SKU

	t.Run("shop admin manages suppliers", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/suppliers", map[string]any{
			"name": "Acme Textiles", "contact": map[string]string{"email": "sales@acme.test"},
		}, &sup))
		assert.Equal(t, "shop-a", sup.TenantID)
		assert.Equal(t, api.Status("active"), sup.Status)
		assert.Equal(t, "sales@acme.test", sup.Contact["email"])
		assert.Equal(t, 1, eventsOf("shop-a", "product.supplier.created.v1"))

		assert.Equal(t, http.StatusForbidden, sys.do("POST", "/admin/suppliers", map[string]any{"name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, sys.do("GET", "/admin/suppliers", nil, nil))
		assert.Equal(t, http.StatusBadRequest, as(t, srv, "_platform", "shop_admin").do("POST", "/admin/suppliers",
			map[string]any{"name": "X"}, nil), "a platform tenant is not a shop")
		other := as(t, srv, "shop-b", "shop_admin")
		assert.Equal(t, http.StatusNotFound, other.do("GET", "/admin/suppliers/"+sup.SupplierID.String(), nil, nil))
		assert.Equal(t, http.StatusNotFound, sys.do("POST", "/sysadmin/suppliers", map[string]any{"name": "X"}, nil))

		var dormant api.Supplier
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/suppliers",
			map[string]any{"name": "Dormant Mill", "status": "inactive"}, &dormant))
		var list api.SupplierList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/suppliers", nil, &list))
		require.Len(t, list.Items, 2)
		assert.Equal(t, dormant.SupplierID, list.Items[0].SupplierID, "newest first, inactive included")
		require.Equal(t, http.StatusOK, other.do("GET", "/admin/suppliers", nil, &list))
		assert.Empty(t, list.Items)

		var got api.Supplier
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/suppliers/"+sup.SupplierID.String(), nil, &got))
		assert.Equal(t, sup.SupplierID, got.SupplierID)

		var renamed api.Supplier
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/suppliers/"+sup.SupplierID.String(),
			map[string]any{"name": "Acme Textiles Ltd", "status": "active"}, &renamed))
		assert.Equal(t, "Acme Textiles Ltd", renamed.Name)
		assert.Equal(t, 1, eventsOf("shop-a", "product.supplier.updated.v1"))
		assert.Equal(t, http.StatusNotFound, other.do("PUT", "/admin/suppliers/"+sup.SupplierID.String(),
			map[string]any{"name": "Taken", "status": "active"}, nil))

		var public struct{ Items []api.SupplierSummary }
		require.Equal(t, http.StatusOK, visitor.do("GET", "/suppliers", nil, &public))
		assert.Equal(t, []api.SupplierSummary{{SupplierID: sup.SupplierID, Name: "Acme Textiles Ltd"}}, public.Items)
		require.Equal(t, http.StatusOK, otherShop.do("GET", "/suppliers", nil, &public))
		assert.Empty(t, public.Items)
	})

	t.Run("shop admin builds a category tree", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Apparel", "slug": "apparel"}, &apparel))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "T-Shirts", "slug": "t-shirts", "parent_id": apparel.CategoryID}, &tshirts))
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Dup", "slug": "apparel"}, nil))
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Bad", "slug": "Not A Slug"}, nil))
		assert.Equal(t, http.StatusConflict, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Apparel", "slug": "apparel", "parent_id": tshirts.CategoryID}, nil), "cycle")
		assert.Equal(t, http.StatusForbidden, sys.do("POST", "/admin/categories",
			map[string]any{"name": "X", "slug": "x"}, nil))

		icon := uuid.MustParse("11111111-1111-4111-8111-111111111111")
		replaced := uuid.MustParse("22222222-2222-4222-8222-222222222222")
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Apparel", "slug": "apparel", "image_id": icon}, &apparel))
		require.NotNil(t, apparel.ImageID)
		assert.Equal(t, icon, *apparel.ImageID)
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Apparel", "slug": "apparel", "image_id": "not-a-uuid"}, nil))
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Apparel", "slug": "apparel", "image_id": replaced}, &apparel))
		assert.Equal(t, replaced, *apparel.ImageID)
		var cleared api.Category
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Apparel", "slug": "apparel"}, &cleared), "omitting image_id clears the icon")
		assert.Nil(t, cleared.ImageID)

		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/categories/"+tshirts.CategoryID.String(),
			map[string]any{"name": "T-Shirts", "slug": "t-shirts", "parent_id": apparel.CategoryID, "image_id": icon}, &tshirts))

		var tree struct{ Items []api.CategoryNode }
		require.Equal(t, http.StatusOK, visitor.do("GET", "/categories", nil, &tree))
		require.Len(t, tree.Items, 1)
		assert.Nil(t, tree.Items[0].ImageID)
		require.Len(t, tree.Items[0].Children, 1)
		assert.Equal(t, "t-shirts", tree.Items[0].Children[0].Slug)
		require.NotNil(t, tree.Items[0].Children[0].ImageID)
		assert.Equal(t, icon, *tree.Items[0].Children[0].ImageID)
	})

	t.Run("shop admin curates banners", func(t *testing.T) {
		bg := uuid.MustParse("33333333-3333-4333-8333-333333333333")
		large := uuid.MustParse("44444444-4444-4444-8444-444444444444")
		icon := uuid.MustParse("11111111-1111-4111-8111-111111111111")
		body := func(category uuid.UUID, text string, picture uuid.UUID) map[string]any {
			return map[string]any{
				"category_id": category, "text": text,
				"background_image_id": bg, "large_image_id": picture,
			}
		}
		assert.Equal(t, http.StatusForbidden, visitor.do("POST", "/admin/banners", body(apparel.CategoryID, "New", large), nil))
		assert.Equal(t, http.StatusForbidden, sys.do("POST", "/admin/banners", body(apparel.CategoryID, "New", large), nil))

		var first, second api.Banner
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/banners", body(apparel.CategoryID, "New in", large), &first))
		assert.Equal(t, "Apparel", first.CategoryName)
		assert.Equal(t, 0, first.SortOrder)
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/banners", body(apparel.CategoryID, "Again", large), nil),
			"one banner per category")
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/banners", body(tshirts.CategoryID, "Tees", icon), nil),
			"the large image is not the category icon")
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/banners",
			body(uuid.MustParse("99999999-9999-4999-8999-999999999999"), "Missing", large), nil))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/banners", body(tshirts.CategoryID, "Tees", large), &second))
		assert.Equal(t, "T-Shirts", second.CategoryName)
		assert.Equal(t, 2, eventsOf("shop-a", "product.banner.created.v1"))

		var listed struct{ Items []api.Banner }
		require.Equal(t, http.StatusOK, visitor.do("GET", "/banners", nil, &listed))
		require.Len(t, listed.Items, 2)
		assert.Equal(t, []uuid.UUID{first.BannerID, second.BannerID}, []uuid.UUID{listed.Items[0].BannerID, listed.Items[1].BannerID})
		require.Equal(t, http.StatusOK, otherShop.do("GET", "/banners", nil, &listed))
		assert.Empty(t, listed.Items)

		var tree map[string]any
		require.Equal(t, http.StatusOK, visitor.do("GET", "/categories", nil, &tree))
		raw, err := json.Marshal(tree)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "background_image_id")

		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/categories/"+apparel.CategoryID.String(),
			map[string]any{"name": "Clothing", "slug": "apparel"}, nil))
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/banners/"+first.BannerID.String(), body(apparel.CategoryID, "Just in", large), &first))
		assert.Equal(t, "Just in", first.Text)
		assert.Equal(t, "Clothing", first.CategoryName)
		assert.Equal(t, 0, first.SortOrder, "replacing a banner keeps its place")
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/banners/"+second.BannerID.String(), body(tshirts.CategoryID, "Tees", icon), nil))
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("PUT", "/admin/banners/"+first.BannerID.String(),
			body(apparel.CategoryID, "Taken", large), nil))

		require.Equal(t, http.StatusNoContent, admin.do("PUT", "/admin/banners/order",
			map[string]any{"ids": []string{second.BannerID.String(), first.BannerID.String()}}, nil))
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/banners/order",
			map[string]any{"ids": []string{second.BannerID.String()}}, nil), "the list must be complete")
		require.Equal(t, http.StatusOK, visitor.do("GET", "/banners", nil, &listed))
		require.Len(t, listed.Items, 2)
		assert.Equal(t, second.BannerID, listed.Items[0].BannerID)
		assert.Equal(t, 1, eventsOf("shop-a", "product.banners.reordered.v1"))

		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/banners/"+first.BannerID.String(), nil, nil))
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("DELETE", "/admin/banners/"+second.BannerID.String(), nil, nil))
		require.Equal(t, http.StatusOK, visitor.do("GET", "/banners", nil, &listed))
		require.Len(t, listed.Items, 1)
		assert.Equal(t, second.BannerID, listed.Items[0].BannerID)

		var hats api.Category
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Hats", "slug": "hats"}, &hats))
		var gone api.Banner
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/banners", body(hats.CategoryID, "Caps", large), &gone))
		deleted := eventsOf("shop-a", "product.banner.deleted.v1")
		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/categories/"+hats.CategoryID.String(), nil, nil))
		assert.Equal(t, deleted+1, eventsOf("shop-a", "product.banner.deleted.v1"), "deleting the category removes its banner")
		require.Equal(t, http.StatusOK, visitor.do("GET", "/banners", nil, &listed))
		require.Len(t, listed.Items, 1)
		assert.NotEqual(t, hats.CategoryID, listed.Items[0].CategoryID)
	})

	t.Run("shop admin creates a product with variants and SKUs", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products", map[string]any{
			"name": "Acme Organic T-Shirt", "description": "Soft cotton crew neck",
			"supplier_id": sup.SupplierID, "category_ids": []any{tshirts.CategoryID},
		}, &shirt))
		assert.Equal(t, 1, eventsOf("shop-a", "product.created.v1"))

		var va api.Variant
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products/"+shirt.ProductID.String()+"/variants",
			map[string]any{"axis_name": "colour", "values": []string{"Red", "Blue"}}, &va))
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/products/"+shirt.ProductID.String()+"/variants",
			map[string]any{"axis_name": "colour", "values": []string{"Green"}}, nil))

		skuPath := "/admin/products/" + shirt.ProductID.String() + "/skus"
		require.Equal(t, http.StatusCreated, admin.do("POST", skuPath, map[string]any{
			"sku_code": "TS-RED", "variant_values": map[string]string{"colour": "Red"}, "tax_class": "za-standard",
			"weight_grams": 180, "dimensions_mm": map[string]int{"length": 300, "width": 250, "height": 20},
		}, &red))
		assert.True(t, red.InventoryTracked)
		assert.Equal(t, &api.Dimensions{Length: 300, Width: 250, Height: 20}, red.DimensionsMm)
		require.Equal(t, http.StatusCreated, admin.do("POST", skuPath, map[string]any{
			"sku_code": "TS-BLUE", "variant_values": map[string]string{"colour": "Blue"}, "tax_class": "za-standard",
		}, &blue))

		for name, tc := range map[string]struct {
			body   map[string]any
			status int
		}{
			"duplicate code":   {map[string]any{"sku_code": "TS-RED", "variant_values": map[string]string{"colour": "Blue"}, "tax_class": "za-standard"}, http.StatusConflict},
			"same combination": {map[string]any{"sku_code": "TS-RED-2", "variant_values": map[string]string{"colour": "Red"}, "tax_class": "za-standard"}, http.StatusConflict},
			"unknown value":    {map[string]any{"sku_code": "TS-GRN", "variant_values": map[string]string{"colour": "Green"}, "tax_class": "za-standard"}, http.StatusBadRequest},
			"missing axis":     {map[string]any{"sku_code": "TS-X", "tax_class": "za-standard"}, http.StatusBadRequest},
			"bad tax class":    {map[string]any{"sku_code": "TS-Y", "variant_values": map[string]string{"colour": "Red"}, "tax_class": "Standard Rate"}, http.StatusBadRequest},
			"partial box": {map[string]any{
				"sku_code": "TS-Z", "variant_values": map[string]string{"colour": "Red"}, "tax_class": "za-standard",
				"dimensions_mm": map[string]int{"length": 300},
			}, http.StatusBadRequest},
		} {
			assert.Equal(t, tc.status, admin.do("POST", skuPath, tc.body, nil), name)
		}
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/products/"+shirt.ProductID.String()+"/variants",
			map[string]any{"axis_name": "size", "values": []string{"S"}}, nil), "axis after SKUs")
		assert.Equal(t, 2, eventsOf("shop-a", "product.sku.created.v1"))

		var sku struct {
			Payload map[string]any `json:"payload"`
		}
		var raw []byte
		require.NoError(t, db.QueryRow(ctx, `SELECT envelope FROM outbox WHERE envelope->>'event_type' = 'product.sku.created.v1'
			ORDER BY created_at LIMIT 1`).Scan(&raw))
		require.NoError(t, json.Unmarshal(raw, &sku))
		assert.Equal(t, "za-standard", sku.Payload["tax_class"])
		assert.Equal(t, map[string]any{"colour": "Red"}, sku.Payload["variant_values"])
	})

	t.Run("images: a new primary demotes the old one", func(t *testing.T) {
		path := "/admin/products/" + shirt.ProductID.String() + "/images"
		img1, img2 := "0192f0c1-0000-7000-8000-000000000001", "0192f0c1-0000-7000-8000-000000000002"
		require.Equal(t, http.StatusCreated, admin.do("POST", path, map[string]any{"image_id": img1, "is_primary": true}, nil))
		require.Equal(t, http.StatusCreated, admin.do("POST", path, map[string]any{"image_id": img2, "is_primary": true, "sort_order": 1}, nil))

		var p api.Product
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products/"+shirt.ProductID.String(), nil, &p))
		require.Len(t, p.Images, 2)
		assert.Equal(t, img2, p.Images[0].ImageID.String())
		assert.True(t, p.Images[0].IsPrimary)
		assert.False(t, p.Images[1].IsPrimary)

		assert.Equal(t, http.StatusNoContent, admin.do("DELETE", path+"/"+img1, nil, nil))
		assert.Equal(t, http.StatusNotFound, admin.do("DELETE", path+"/"+img1, nil, nil))
	})

	t.Run("visitors browse and search their shop only", func(t *testing.T) {
		var list api.ProductList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products", nil, &list))
		require.Len(t, list.Items, 1)
		got := list.Items[0]
		assert.Equal(t, "Acme Organic T-Shirt", got.Name)
		assert.Len(t, got.Skus, 2)
		require.NotNil(t, got.PrimaryImageID)

		for q, n := range map[string]int{"shirts": 1, "organic cotton": 1, "TS-BLUE": 1, "blue": 1, "hoodie": 0} {
			require.Equal(t, http.StatusOK, visitor.do("GET", "/products?q="+url.QueryEscape(q), nil, &list))
			assert.Len(t, list.Items, n, q)
		}
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?category="+apparel.CategoryID.String(), nil, &list))
		assert.Len(t, list.Items, 1, "a category includes its subcategories")
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?supplier_id="+sup.SupplierID.String(), nil, &list))
		assert.Len(t, list.Items, 1)

		require.Equal(t, http.StatusOK, otherShop.do("GET", "/products", nil, &list))
		assert.Empty(t, list.Items)
		assert.Equal(t, http.StatusNotFound, otherShop.do("GET", "/products/"+shirt.ProductID.String(), nil, nil))
	})

	t.Run("pagination", func(t *testing.T) {
		for _, name := range []string{"Mug", "Cap"} {
			var p api.Product
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products", map[string]any{"name": name}, &p))
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products/"+p.ProductID.String()+"/skus",
				map[string]any{"sku_code": name, "tax_class": "za-standard"}, nil))
		}
		var page1, page2 api.ProductList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?page_size=2", nil, &page1))
		require.Len(t, page1.Items, 2)
		assert.Equal(t, []string{"Cap", "Mug"}, []string{page1.Items[0].Name, page1.Items[1].Name})
		require.NotEmpty(t, page1.Page.NextCursor)
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?page_size=2&cursor="+page1.Page.NextCursor, nil, &page2))
		require.Len(t, page2.Items, 1)
		assert.Empty(t, page2.Page.NextCursor)
		assert.Equal(t, http.StatusBadRequest, visitor.do("GET", "/products?cursor=garbage", nil, nil))
	})

	t.Run("inactive products are hidden from visitors", func(t *testing.T) {
		var p api.Product
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/products/"+shirt.ProductID.String(), map[string]any{
			"name": "Acme Organic T-Shirt", "status": "inactive", "supplier_id": sup.SupplierID,
			"category_ids": []any{tshirts.CategoryID},
		}, &p))
		assert.Equal(t, api.Status("inactive"), p.Status)
		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/products/"+shirt.ProductID.String(), nil, nil))
		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/skus/"+red.SkuID.String(), nil, nil))
		assert.Equal(t, http.StatusOK, admin.do("GET", "/products/"+shirt.ProductID.String(), nil, nil))
		var list api.ProductList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?q=shirt", nil, &list))
		assert.Empty(t, list.Items)

		p.Status = "active"
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/products/"+shirt.ProductID.String(), map[string]any{
			"name": "Acme Organic T-Shirt", "supplier_id": sup.SupplierID,
		}, nil))
		var sku api.SKU
		require.Equal(t, http.StatusOK, visitor.do("GET", "/skus/"+red.SkuID.String(), nil, &sku))
		assert.Equal(t, "TS-RED", sku.SkuCode)
	})

	t.Run("search ranks by orders, base prices and ratings per role", func(t *testing.T) {
		validator, err := events.NewValidator()
		require.NoError(t, err)
		upstream, err := eventbus.NewClient(ctx, eventbus.Config{
			NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
			Streams: []eventbus.StreamConfig{
				eventbus.OrderStream(), eventbus.ReviewStream(), eventbus.PricingStream(), eventbus.InventoryStream(),
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = upstream.Close(context.Background()) })
		publish := func(eventType string, payload any) {
			t.Helper()
			require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
				EventType: eventType, TenantID: "shop-a",
				Actor: eventbus.Actor{Type: "service", ID: "yc-upstream"}, Payload: payload,
			}))
		}
		names := func(c client, query string) (api.ProductList, []string) {
			t.Helper()
			var list api.ProductList
			require.Equal(t, http.StatusOK, c.do("GET", "/products"+query, nil, &list))
			out := make([]string, len(list.Items))
			for i, it := range list.Items {
				out[i] = it.Name
			}
			return list, out
		}

		list, byNewest := names(admin, "")
		assert.Equal(t, api.Sort("newest"), list.Sort, "shop admins see the newest first")
		require.Equal(t, []string{"Cap", "Mug", "Acme Organic T-Shirt"}, byNewest)
		sku, productOf := map[string]string{}, map[string]string{}
		for _, it := range list.Items {
			sku[it.Name] = it.Skus[0].SkuID.String()
			productOf[it.Name] = it.ProductID.String()
		}

		customer := as(t, srv, "shop-a", "customer")
		list, _ = names(customer, "")
		require.NotNil(t, list.Facets.Favourites, "customers get the favourites facet")
		assert.Equal(t, 0, list.Facets.Favourites.ProductCount)

		zar := events.Money{AmountMinor: 100, Currency: "ZAR"}
		placed := func(userID, userType string, products ...string) {
			o := events.OrderPlacedV1{
				OrderID: uuid.NewString(), OrderNumber: "SHOP-A-1", UserID: userID, UserType: userType,
				CheckoutSessionID: uuid.NewString(), ItemsTotal: zar, Shipping: zar, TaxTotal: zar, Total: zar,
				PlacedAt: time.Now().UTC(),
			}
			if userType == "visitor" {
				o.GuestContact = &events.GuestContact{Email: "guest@example.com", PhoneE164: "+27820000000"}
			}
			for _, name := range products {
				o.Items = append(o.Items, events.OrderPlacedV1ItemsItem{
					SKUID: sku[name], SKUCode: name, NameSnapshot: name, Quantity: 5, UnitPrice: zar, LineTotal: zar,
				})
			}
			publish(events.TypeOrderPlacedV1, o)
		}
		placed("00000000-0000-0000-0000-0000000000a1", "customer", "Mug")
		placed(uuid.NewString(), "visitor", "Mug", "Acme Organic T-Shirt")
		require.Eventually(t, func() bool {
			_, got := names(visitor, "")
			return assert.ObjectsAreEqual([]string{"Mug", "Acme Organic T-Shirt", "Cap"}, got)
		}, 10*time.Second, 50*time.Millisecond, "visitors see popular products first")
		list, _ = names(visitor, "")
		assert.Equal(t, api.Sort("popular"), list.Sort)
		assert.Nil(t, list.Facets.Favourites)
		list, favourites := names(customer, "?favourites=true")
		assert.Equal(t, []string{"Mug"}, favourites, "a customer's favourites are the products they ordered")
		require.NotNil(t, list.Facets.Favourites)
		assert.Equal(t, 1, list.Facets.Favourites.ProductCount, "visitors' orders are not anyone's favourites")
		assert.Equal(t, http.StatusBadRequest, visitor.do("GET", "/products?favourites=true", nil, nil))

		publish(events.TypeIdentityUserDeletedV1, events.IdentityUserDeletedV1{
			UserID: "00000000-0000-0000-0000-0000000000a1", Pseudonym: uuid.NewString(), Role: "customer",
			RequestedBy: "user", DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			list, _ := names(customer, "")
			return list.Facets.Favourites.ProductCount == 0
		}, 10*time.Second, 50*time.Millisecond, "a deleted customer's favourites go (GDPR)")
		_, popular := names(visitor, "")
		assert.Equal(t, []string{"Mug", "Acme Organic T-Shirt", "Cap"}, popular, "popularity stays")

		stats := func(name string) api.ReviewStats {
			t.Helper()
			var l api.ProductList
			require.Equal(t, http.StatusOK, visitor.do("GET", "/products", nil, &l))
			for _, it := range l.Items {
				if it.Name == name {
					return it.Reviews
				}
			}
			t.Fatalf("no product %s", name)
			return api.ReviewStats{}
		}
		assert.Equal(t, api.ReviewStats{}, stats("Cap"), "no reviews yet")
		reviewed := func(eventType, name string, count, sum, likes, version int64) {
			s := events.ProductReviewStats{RatingCount: count, RatingSum: sum, LikeCount: likes, Version: version}
			at := time.Now().UTC()
			switch eventType {
			case events.TypeReviewApprovedV1:
				publish(eventType, events.ReviewApprovedV1{
					ReviewID: uuid.NewString(), ProductID: productOf[name], UserID: uuid.NewString(), Rating: 5,
					ApprovedAt: at, ProductStats: s,
				})
			case events.TypeReviewLikeAddedV1:
				publish(eventType, events.ReviewLikeAddedV1{
					ProductID: productOf[name], UserID: uuid.NewString(), LikedAt: at, ProductStats: s,
				})
			}
		}
		reviewed(events.TypeReviewApprovedV1, "Cap", 2, 9, 1, 3)
		reviewed(events.TypeReviewApprovedV1, "Acme Organic T-Shirt", 1, 5, 0, 1)
		reviewed(events.TypeReviewLikeAddedV1, "Mug", 0, 0, 2, 1)
		require.Eventually(t, func() bool {
			_, got := names(visitor, "?sort=rating")
			return assert.ObjectsAreEqual([]string{"Acme Organic T-Shirt", "Cap", "Mug"}, got)
		}, 10*time.Second, 50*time.Millisecond, "best rated first; unrated last")
		avg := 4.5
		assert.Equal(t, api.ReviewStats{RatingCount: 2, RatingAverage: &avg, LikeCount: 1}, stats("Cap"))
		assert.Equal(t, api.ReviewStats{LikeCount: 2}, stats("Mug"))

		reviewed(events.TypeReviewLikeAddedV1, "Cap", 1, 1, 0, 2)
		time.Sleep(500 * time.Millisecond)
		assert.Equal(t, 2, stats("Cap").RatingCount, "older stats versions are ignored")

		var rated1, rated2 api.ProductList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?sort=rating&page_size=2", nil, &rated1))
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?sort=rating&page_size=2&cursor="+rated1.Page.NextCursor, nil, &rated2))
		require.Len(t, rated2.Items, 1)
		assert.Equal(t, "Mug", rated2.Items[0].Name)

		var page1, page2 api.ProductList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?page_size=2", nil, &page1))
		require.NotEmpty(t, page1.Page.NextCursor)
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?page_size=2&cursor="+page1.Page.NextCursor, nil, &page2))
		require.Len(t, page2.Items, 1)
		assert.Equal(t, "Cap", page2.Items[0].Name)
		assert.Equal(t, http.StatusBadRequest,
			visitor.do("GET", "/products?sort=newest&cursor="+page1.Page.NextCursor, nil, nil), "cursor of another sort")
		assert.Equal(t, http.StatusBadRequest, visitor.do("GET", "/products?sort=cheapest", nil, nil))

		price := func(name string, minor int64, at time.Time) {
			publish(events.TypePricingBasePriceSetV1, events.PricingBasePriceSetV1{
				SKUID: sku[name], Price: events.Money{AmountMinor: minor, Currency: "ZAR"}, ValidFrom: at.UTC(),
			})
		}
		now := time.Now()
		price("Acme Organic T-Shirt", 15000, now)
		price("Cap", 5000, now)
		price("Mug", 9000, now)
		publish(events.TypePricingBasePriceRemovedV1, events.PricingBasePriceRemovedV1{
			SKUID: sku["Mug"], RemovedAt: now.Add(time.Second).UTC(),
		})
		require.Eventually(t, func() bool {
			_, got := names(visitor, "?sort=price_asc")
			return assert.ObjectsAreEqual([]string{"Cap", "Acme Organic T-Shirt", "Mug"}, got)
		}, 10*time.Second, 50*time.Millisecond, "unpriced products come last")
		_, got := names(visitor, "?sort=price_desc")
		assert.Equal(t, []string{"Acme Organic T-Shirt", "Cap", "Mug"}, got)

		price("Cap", 99999, now.Add(-time.Hour))
		price("Mug", 1, now.Add(-time.Hour))
		time.Sleep(500 * time.Millisecond)
		_, got = names(visitor, "?sort=price_asc")
		assert.Equal(t, []string{"Cap", "Acme Organic T-Shirt", "Mug"}, got, "older price events are ignored")

		require.Equal(t, http.StatusOK, visitor.do("GET", "/products?sort=price_asc&page_size=1", nil, &page1))
		require.Equal(t, http.StatusOK,
			visitor.do("GET", "/products?sort=price_asc&page_size=1&cursor="+page1.Page.NextCursor, nil, &page2))
		assert.Equal(t, "Acme Organic T-Shirt", page2.Items[0].Name)

		inStock := func() map[string]bool {
			var l api.ProductList
			require.Equal(t, http.StatusOK, visitor.do("GET", "/products", nil, &l))
			out := map[string]bool{}
			for _, it := range l.Items {
				out[it.Name] = it.InStock
			}
			return out
		}
		assert.Equal(t, map[string]bool{"Cap": true, "Mug": true, "Acme Organic T-Shirt": true}, inStock(),
			"no stock signal yet: in stock")
		publish(events.TypeInventoryStockDepletedV1, events.InventoryStockDepletedV1{
			SKUID: sku["Cap"], OnHand: 0, Reserved: 0, Available: 0, DepletedAt: now.UTC(),
		})
		require.Eventually(t, func() bool { return !inStock()["Cap"] }, 10*time.Second, 50*time.Millisecond)
		_, got = names(visitor, "?in_stock=true")
		assert.Equal(t, []string{"Mug", "Acme Organic T-Shirt"}, got)
		publish(events.TypeInventoryStockRestoredV1, events.InventoryStockRestoredV1{
			SKUID: sku["Cap"], OnHand: 3, Reserved: 0, Available: 3, RestoredAt: now.Add(-time.Minute).UTC(),
		})
		publish(events.TypeInventoryStockRestoredV1, events.InventoryStockRestoredV1{
			SKUID: sku["Mug"], OnHand: 3, Reserved: 0, Available: 3, RestoredAt: now.UTC(),
		})
		time.Sleep(500 * time.Millisecond)
		assert.False(t, inStock()["Cap"], "an older restore is ignored")

		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/skus/"+sku["Cap"],
			map[string]any{"sku_code": "Cap", "tax_class": "za-standard", "inventory_tracked": false}, nil))
		assert.True(t, inStock()["Cap"], "an untracked SKU is always in stock")
	})

	t.Run("internal SKU batch", func(t *testing.T) {
		svc := as(t, srv, "shop-a", "service").withCaller()
		missing := uuid.NewString()
		var batch api.SKUBatch
		require.Equal(t, http.StatusOK, svc.do("GET", "/internal/skus/batch?ids="+red.SkuID.String()+","+missing, nil, &batch))
		require.Len(t, batch.Skus, 1)
		d := batch.Skus[0]
		assert.Equal(t, "Acme Organic T-Shirt", d.ProductName)
		assert.Equal(t, red.SkuCode, d.SkuCode)
		assert.Equal(t, api.SKUDetailStatusActive, d.Status)
		assert.True(t, d.InventoryTracked)
		assert.False(t, d.IsDigital)
		assert.Equal(t, 180, *d.WeightGrams)
		assert.Equal(t, &api.Dimensions{Length: 300, Width: 250, Height: 20}, d.DimensionsMm)
		assert.Equal(t, missing, batch.Missing[0].String())

		assert.NotEqual(t, http.StatusOK, visitor.do("GET", "/internal/skus/batch?ids="+red.SkuID.String(), nil, nil),
			"service callers only")
		assert.Equal(t, http.StatusOK, as(t, srv, "shop-b", "service").withCaller().do("GET", "/internal/skus/batch?ids="+red.SkuID.String(), nil, &batch))
		assert.Empty(t, batch.Skus, "tenant-scoped")
	})

	t.Run("roles are enforced", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, as(t, srv, "shop-a", "").do("POST", "/admin/products", map[string]any{"name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, visitor.do("POST", "/admin/products", map[string]any{"name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, sys.do("POST", "/admin/products", map[string]any{"name": "X"}, nil),
			"system admins name the shop; /admin acts on the caller's own")
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("DELETE", "/admin/skus/"+red.SkuID.String(), nil, nil))
	})

	t.Run("multi-category browse ANDs positives and excludes", func(t *testing.T) {
		var lifestyle, allergens, vegan, nuts api.Category
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Lifestyle", "slug": "lifestyle"}, &lifestyle))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Allergens", "slug": "allergens"}, &allergens))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Vegan", "slug": "vegan", "parent_id": lifestyle.CategoryID}, &vegan))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/categories",
			map[string]any{"name": "Nuts", "slug": "nuts", "parent_id": allergens.CategoryID}, &nuts))

		mk := func(name string, cats ...uuid.UUID) {
			t.Helper()
			ids := make([]any, len(cats))
			for i, c := range cats {
				ids[i] = c
			}
			var p api.Product
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products",
				map[string]any{"name": name, "category_ids": ids}, &p))
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products/"+p.ProductID.String()+"/skus",
				map[string]any{"sku_code": name, "tax_class": "za-standard"}, nil))
		}
		mk("Vegan Bowl", vegan.CategoryID)
		mk("Nut Mix", nuts.CategoryID)
		mk("Vegan Nut Bar", vegan.CategoryID, nuts.CategoryID)
		mk("Plain Rice")

		names := func(path string) []string {
			t.Helper()
			var list api.ProductList
			require.Equal(t, http.StatusOK, visitor.do("GET", path, nil, &list))
			out := make([]string, len(list.Items))
			for i, it := range list.Items {
				out[i] = it.Name
			}
			return out
		}
		assert.ElementsMatch(t, []string{"Vegan Bowl", "Vegan Nut Bar"},
			names("/products?category="+vegan.CategoryID.String()))
		assert.ElementsMatch(t, []string{"Vegan Bowl", "Vegan Nut Bar"},
			names("/products?category="+lifestyle.CategoryID.String()), "parent includes descendants")
		assert.Equal(t, []string{"Vegan Nut Bar"},
			names("/products?category="+vegan.CategoryID.String()+"&category="+nuts.CategoryID.String()), "AND")
		assert.Equal(t, []string{"Vegan Bowl"},
			names("/products?category="+vegan.CategoryID.String()+"&exclude_category="+nuts.CategoryID.String()))
		assert.NotContains(t, names("/products?exclude_category="+nuts.CategoryID.String()), "Nut Mix")
		assert.NotContains(t, names("/products?exclude_category="+nuts.CategoryID.String()), "Vegan Nut Bar")
		assert.Contains(t, names("/products?exclude_category="+nuts.CategoryID.String()), "Vegan Bowl")

		unknown := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		assert.Empty(t, names("/products?category="+unknown.String()), "unknown positive id matches nothing")

		q := "/products?"
		for i := 0; i < 17; i++ {
			if i > 0 {
				q += "&"
			}
			q += "category=" + uuid.MustParse(fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", i+1)).String()
		}
		assert.Equal(t, http.StatusBadRequest, visitor.do("GET", q, nil, nil), "cap of 16")
	})

	// merch creates a product of shop-a with one SKU per code.
	type merch struct {
		product api.Product
		skus    []string
	}
	merchandise := func(c client, name string, codes ...string) merch {
		t.Helper()
		var m merch
		require.Equal(t, http.StatusCreated, c.do("POST", "/admin/products", map[string]any{"name": name}, &m.product))
		if len(codes) > 1 { // SKUs of one product need distinct variant values
			require.Equal(t, http.StatusCreated, c.do("POST", "/admin/products/"+m.product.ProductID.String()+"/variants",
				map[string]any{"axis_name": "pack", "values": codes}, nil))
		}
		for _, code := range codes {
			body := map[string]any{"sku_code": code, "tax_class": "za-standard"}
			if len(codes) > 1 {
				body["variant_values"] = map[string]string{"pack": code}
			}
			var s api.SKU
			require.Equal(t, http.StatusCreated, c.do("POST", "/admin/products/"+m.product.ProductID.String()+"/skus", body, &s))
			m.skus = append(m.skus, s.SkuID.String())
		}
		return m
	}
	setStatus := func(m merch, status string) {
		t.Helper()
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/products/"+m.product.ProductID.String(),
			map[string]any{"name": m.product.Name, "status": status}, nil))
	}
	idOf := func(m merch) string { return m.product.ProductID.String() }

	t.Run("shop admin curates related products", func(t *testing.T) {
		src := merchandise(admin, "Rel Source", "REL-SRC")
		a := merchandise(admin, "Rel A", "REL-A")
		b := merchandise(admin, "Rel B", "REL-B")
		dormant := merchandise(admin, "Rel Dormant", "REL-D")
		doomed := merchandise(admin, "Rel Doomed", "REL-X")
		setStatus(dormant, "inactive")
		foreign := merchandise(as(t, srv, "shop-b", "shop_admin"), "Other Shop Product", "OTHER-REL")

		path := "/admin/products/" + idOf(src) + "/related"
		put := func(c client, ids ...string) (int, api.RelatedProducts) {
			t.Helper()
			var out api.RelatedProducts
			status := c.do("PUT", path, map[string]any{"product_ids": ids}, &out)
			return status, out
		}
		manyIDs := func(n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = uuid.NewString()
			}
			return out
		}
		toStrings := func(ids []uuid.UUID) []string {
			out := make([]string, len(ids))
			for i, id := range ids {
				out[i] = id.String()
			}
			return out
		}
		related := func(c client, id string) []string {
			t.Helper()
			var p api.Product
			require.Equal(t, http.StatusOK, c.do("GET", "/products/"+id, nil, &p))
			require.NotNil(t, p.RelatedProductIds)
			return toStrings(p.RelatedProductIds)
		}
		customer := as(t, srv, "shop-a", "customer")
		stored := func(id string) int {
			var n int
			require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM product_related
					WHERE product_id = $1 OR related_product_id = $1`, id).Scan(&n)
			}))
			return n
		}

		status, _ := put(visitor, idOf(a))
		assert.Equal(t, http.StatusForbidden, status)
		status, _ = put(sys, idOf(a))
		assert.Equal(t, http.StatusForbidden, status, "system admins cannot use /admin")
		assert.Empty(t, related(visitor, idOf(src)), "no related products yet")
		var raw map[string]json.RawMessage
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products/"+idOf(src), nil, &raw))
		assert.JSONEq(t, `[]`, string(raw["related_product_ids"]), "the field is always present")

		status, got := put(admin, idOf(b), idOf(a), idOf(dormant), idOf(doomed))
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant), idOf(doomed)}, toStrings(got.ProductIds))
		assert.Equal(t, 1, eventsOf("shop-a", "product.related.updated.v1"))
		var evt []byte
		require.NoError(t, db.QueryRow(ctx, `SELECT envelope->'payload' FROM outbox
			WHERE envelope->>'event_type' = 'product.related.updated.v1'`).Scan(&evt))
		var payload events.ProductRelatedUpdatedV1
		require.NoError(t, json.Unmarshal(evt, &payload))
		assert.Equal(t, idOf(src), payload.ProductID)
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant), idOf(doomed)}, payload.RelatedProductIds)

		for name, ids := range map[string][]string{
			"itself":        {idOf(a), idOf(src)},
			"unknown":       {uuid.NewString()},
			"another shop":  {idOf(foreign)},
			"not a UUID":    {"nope"},
			"twenty-one":    manyIDs(21),
			"one bad entry": {idOf(a), idOf(foreign), idOf(b)},
		} {
			status, _ := put(admin, ids...)
			assert.Equal(t, http.StatusBadRequest, status, name)
		}
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant), idOf(doomed)}, related(admin, idOf(src)),
			"a rejected list changes nothing")
		assert.Equal(t, 1, eventsOf("shop-a", "product.related.updated.v1"), "and publishes nothing")

		missing := "/admin/products/" + uuid.NewString() + "/related"
		assert.Equal(t, http.StatusNotFound, admin.do("PUT", missing, map[string]any{"product_ids": []string{idOf(a)}}, nil))
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("PUT", path,
			map[string]any{"product_ids": []string{idOf(foreign)}}, nil), "another shop's admin cannot reach the product")

		for _, c := range []client{visitor, customer} {
			assert.Equal(t, []string{idOf(b), idOf(a), idOf(doomed)}, related(c, idOf(src)),
				"shoppers get active targets in the admin's order")
		}
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant), idOf(doomed)}, related(admin, idOf(src)),
			"admins get the stored list, inactive included")
		assert.Empty(t, related(visitor, idOf(a)), "links are directed")
		var updated api.Product
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/products/"+idOf(src),
			map[string]any{"name": src.product.Name}, &updated))
		assert.Len(t, updated.RelatedProductIds, 4, "updating the product keeps its related list")

		setStatus(dormant, "active")
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant), idOf(doomed)}, related(visitor, idOf(src)),
			"a target shows once it is active")
		setStatus(dormant, "inactive")

		status, got = put(admin, idOf(a), idOf(a), idOf(b))
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{idOf(a), idOf(b)}, toStrings(got.ProductIds), "repeats collapse")
		status, _ = put(admin, idOf(b), idOf(a), idOf(dormant), idOf(doomed))
		require.Equal(t, http.StatusOK, status)

		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/products/"+idOf(doomed), nil, nil))
		assert.Equal(t, 0, stored(idOf(doomed)), "deleting a target removes the links to it")
		assert.Equal(t, []string{idOf(b), idOf(a)}, related(visitor, idOf(src)))
		assert.Equal(t, []string{idOf(b), idOf(a), idOf(dormant)}, related(admin, idOf(src)))
		status, _ = put(admin, idOf(doomed))
		assert.Equal(t, http.StatusBadRequest, status, "a deleted product cannot be a target")

		holder := merchandise(admin, "Rel Holder", "REL-H")
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/products/"+idOf(holder)+"/related",
			map[string]any{"product_ids": []string{idOf(a)}}, nil))
		assert.Equal(t, 1, stored(idOf(holder)))
		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/products/"+idOf(holder), nil, nil))
		assert.Equal(t, 0, stored(idOf(holder)), "deleting the product removes its links")
		assert.Equal(t, http.StatusNotFound, admin.do("PUT", "/admin/products/"+idOf(holder)+"/related",
			map[string]any{"product_ids": []string{}}, nil))

		status, got = put(admin)
		require.Equal(t, http.StatusOK, status)
		assert.Empty(t, got.ProductIds, "an empty list clears")
		assert.Empty(t, related(admin, idOf(src)))
		assert.Equal(t, 0, stored(idOf(src)))
	})

	t.Run("also-bought ranks products ordered together", func(t *testing.T) {
		seed := merchandise(admin, "AB Seed", "AB-SEED-1", "AB-SEED-2")
		q := merchandise(admin, "AB Q", "AB-Q")
		r := merchandise(admin, "AB R", "AB-R")
		cold := merchandise(admin, "AB Cold", "AB-COLD")
		merchandise(admin, "AB Never", "AB-NEVER")

		validator, err := events.NewValidator()
		require.NoError(t, err)
		upstream, err := eventbus.NewClient(ctx, eventbus.Config{
			NATSURL: natsURL, ServiceName: "yc-upstream-also-bought", Validator: validator,
			Streams: []eventbus.StreamConfig{eventbus.OrderStream()},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = upstream.Close(context.Background()) })
		zar := events.Money{AmountMinor: 100, Currency: "ZAR"}
		placed := func(userID, userType string, skus ...string) {
			t.Helper()
			o := events.OrderPlacedV1{
				OrderID: uuid.NewString(), OrderNumber: "SHOP-A-AB", UserID: userID, UserType: userType,
				CheckoutSessionID: uuid.NewString(), ItemsTotal: zar, Shipping: zar, TaxTotal: zar, Total: zar,
				PlacedAt: time.Now().UTC(),
			}
			if userType == "visitor" {
				o.GuestContact = &events.GuestContact{Email: "guest@example.com", PhoneE164: "+27820000000"}
			}
			for _, id := range skus {
				o.Items = append(o.Items, events.OrderPlacedV1ItemsItem{
					SKUID: id, SKUCode: id, NameSnapshot: id, Quantity: 5, UnitPrice: zar, LineTotal: zar,
				})
			}
			require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
				EventType: events.TypeOrderPlacedV1, TenantID: "shop-a",
				Actor: eventbus.Actor{Type: "service", ID: "yc-upstream"}, Payload: o,
			}))
		}
		pair := func(a, b merch) int {
			var n int
			err := dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT order_count FROM product_also_bought
					WHERE product_id = $1 AND other_product_id = $2`, idOf(a), idOf(b)).Scan(&n)
			})
			if err != nil {
				return 0
			}
			return n
		}
		customerID := "00000000-0000-0000-0000-0000000000b2"
		placed(customerID, "customer", q.skus[0], seed.skus[0])
		placed(uuid.NewString(), "visitor", seed.skus[0], q.skus[0], r.skus[0])
		placed(uuid.NewString(), "visitor", r.skus[0], seed.skus[0])
		placed(customerID, "customer", seed.skus[0], seed.skus[1], q.skus[0], q.skus[0])
		placed(customerID, "customer", cold.skus[0])
		require.Eventually(t, func() bool {
			return pair(seed, q) == 3 && pair(seed, r) == 2 && pair(r, q) == 1
		}, 10*time.Second, 50*time.Millisecond, "pairs are counted once per order, guests and customers alike")
		assert.Equal(t, 3, pair(q, seed), "both directions are counted")
		assert.Equal(t, 2, pair(r, seed))
		assert.Equal(t, 1, pair(q, r))
		assert.Zero(t, pair(seed, seed), "a product is not paired with itself")
		assert.Zero(t, pair(cold, seed), "a one-product order pairs nothing")

		alsoBought := func(c client, id merch, query string) (int, []string) {
			t.Helper()
			var out api.AlsoBoughtList
			status := c.do("GET", "/products/"+idOf(id)+"/also-bought"+query, nil, &out)
			names := make([]string, len(out.Items))
			for i, it := range out.Items {
				names[i] = it.Name
			}
			return status, names
		}
		status, names := alsoBought(visitor, seed, "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"AB Q", "AB R"}, names, "most orders first; the seed is absent")
		status, names = alsoBought(visitor, q, "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"AB Seed", "AB R"}, names)

		var full api.AlsoBoughtList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products/"+idOf(seed)+"/also-bought", nil, &full))
		require.NotEmpty(t, full.Items)
		assert.Len(t, full.Items[0].Skus, 1, "items are product summaries")
		assert.Equal(t, q.product.ProductID, full.Items[0].ProductID)

		status, names = alsoBought(visitor, seed, "?limit=1")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"AB Q"}, names, "limit is respected")
		for _, bad := range []string{"0", "-1", "25", "many"} {
			status, _ = alsoBought(visitor, seed, "?limit="+bad)
			assert.Equal(t, http.StatusBadRequest, status, "limit="+bad)
		}
		status, _ = alsoBought(visitor, seed, "?limit=24")
		assert.Equal(t, http.StatusOK, status)

		var empty map[string]json.RawMessage
		require.Equal(t, http.StatusOK, visitor.do("GET", "/products/"+idOf(cold)+"/also-bought", nil, &empty))
		assert.JSONEq(t, `[]`, string(empty["items"]), "a cold product gets an empty list")

		setStatus(r, "inactive")
		_, names = alsoBought(visitor, seed, "")
		assert.Equal(t, []string{"AB Q"}, names, "inactive products are omitted")
		setStatus(r, "active")
		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/products/"+idOf(q), nil, nil))
		_, names = alsoBought(visitor, seed, "")
		assert.Equal(t, []string{"AB R"}, names, "deleted products are omitted")

		status, _ = alsoBought(otherShop, seed, "")
		assert.Equal(t, http.StatusNotFound, status, "another shop's product")
		unknown := merch{product: api.Product{ProductID: uuid.New()}}
		status, _ = alsoBought(visitor, unknown, "")
		assert.Equal(t, http.StatusNotFound, status)
		setStatus(seed, "inactive")
		status, _ = alsoBought(visitor, seed, "")
		assert.Equal(t, http.StatusNotFound, status, "an inactive seed is hidden from visitors")
		status, names = alsoBought(admin, seed, "")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"AB R"}, names)
	})

	t.Run("deletes", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/skus/"+blue.SkuID.String(), nil, nil))
		assert.Equal(t, 1, eventsOf("shop-a", "product.sku.deleted.v1"))
		require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/products/"+shirt.ProductID.String()+"/skus", map[string]any{
			"sku_code": "TS-BLUE", "variant_values": map[string]string{"colour": "Blue"}, "tax_class": "za-standard",
		}, nil), "a deleted SKU releases its code and combination")

		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/products/"+shirt.ProductID.String(), nil, nil))
		var raw []byte
		require.NoError(t, db.QueryRow(ctx, `SELECT envelope->'payload'->'sku_ids' FROM outbox
			WHERE envelope->>'event_type' = 'product.deleted.v1'
			AND envelope->'payload'->>'product_id' = $1`, shirt.ProductID.String()).Scan(&raw))
		var skuIDs []string
		require.NoError(t, json.Unmarshal(raw, &skuIDs))
		assert.Len(t, skuIDs, 2)
		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/products/"+shirt.ProductID.String(), nil, nil))

		assert.Equal(t, http.StatusConflict, admin.do("DELETE", "/admin/suppliers/"+sup.SupplierID.String(), nil, nil),
			"products still reference the supplier")
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("DELETE", "/admin/suppliers/"+sup.SupplierID.String(), nil, nil))
		assert.Equal(t, http.StatusConflict, admin.do("DELETE", "/admin/categories/"+apparel.CategoryID.String(), nil, nil),
			"has subcategories")
		assert.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/categories/"+tshirts.CategoryID.String(), nil, nil))
	})

	t.Run("readyz", func(t *testing.T) {
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, visitor.do("GET", "/readyz", nil, &r))
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}
