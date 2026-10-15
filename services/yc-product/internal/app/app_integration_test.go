//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
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
	t.Setenv("NATS_URL", testkit.NATS(t))
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

	t.Run("system admin creates a supplier for a shop", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, sys.do("POST", "/sysadmin/suppliers", map[string]any{
			"tenant_id": "shop-a", "name": "Acme Textiles", "contact": map[string]string{"email": "sales@acme.test"},
		}, &sup))
		assert.Equal(t, "shop-a", sup.TenantID)
		assert.Equal(t, api.Status("active"), sup.Status)
		assert.Equal(t, 1, eventsOf("shop-a", "product.supplier.created.v1"))

		assert.Equal(t, http.StatusBadRequest, sys.do("POST", "/sysadmin/suppliers",
			map[string]any{"tenant_id": "_platform", "name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, admin.do("POST", "/sysadmin/suppliers",
			map[string]any{"tenant_id": "shop-a", "name": "X"}, nil))

		var list api.SupplierList
		require.Equal(t, http.StatusOK, sys.do("GET", "/sysadmin/suppliers?tenant_id=shop-a", nil, &list))
		require.Len(t, list.Items, 1)

		var renamed api.Supplier
		require.Equal(t, http.StatusOK, sys.do("PUT", "/sysadmin/suppliers/"+sup.SupplierID.String(),
			map[string]any{"name": "Acme Textiles Ltd", "status": "active"}, &renamed))
		assert.Equal(t, "Acme Textiles Ltd", renamed.Name)
		assert.Equal(t, 1, eventsOf("shop-a", "product.supplier.updated.v1"))

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

		var tree struct{ Items []api.CategoryNode }
		require.Equal(t, http.StatusOK, visitor.do("GET", "/categories", nil, &tree))
		require.Len(t, tree.Items, 1)
		require.Len(t, tree.Items[0].Children, 1)
		assert.Equal(t, "t-shirts", tree.Items[0].Children[0].Slug)
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
		}, &red))
		assert.True(t, red.InventoryTracked)
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

	t.Run("roles are enforced", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, as(t, srv, "shop-a", "").do("POST", "/admin/products", map[string]any{"name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, visitor.do("POST", "/admin/products", map[string]any{"name": "X"}, nil))
		assert.Equal(t, http.StatusForbidden, sys.do("POST", "/admin/products", map[string]any{"name": "X"}, nil),
			"system admins name the shop; /admin acts on the caller's own")
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-b", "shop_admin").do("DELETE", "/admin/skus/"+red.SkuID.String(), nil, nil))
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
			WHERE envelope->>'event_type' = 'product.deleted.v1'`).Scan(&raw))
		var skuIDs []string
		require.NoError(t, json.Unmarshal(raw, &skuIDs))
		assert.Len(t, skuIDs, 2)
		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/products/"+shirt.ProductID.String(), nil, nil))

		assert.Equal(t, http.StatusConflict, sys.do("DELETE", "/sysadmin/suppliers/"+sup.SupplierID.String(), nil, nil),
			"products still reference the supplier")
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
