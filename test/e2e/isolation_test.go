//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestIsolation probes the auth boundary through yc-gateway with two shops
// (ADR-017, ADR-020, ADR-021): a shop's credentials work only on its own
// host, its resources are invisible to other shops even by ID, each role
// reaches only its routes, and nothing a client sends can widen that.
// Needs the full stack (task devstack:up && task devstack:app).
func TestIsolation(t *testing.T) {
	s := newStack(t)
	run := time.Now().Unix()
	tenantB := fmt.Sprintf("e2e-iso-b-%d", run)
	shopA := s.provision(t, fmt.Sprintf("e2e-iso-a-%d", run))
	shopB := s.provision(t, tenantB)
	adminA, adminB := s.shopAdmin(t, shopA), s.shopAdmin(t, shopB)
	customerA, _ := shopA.register(t, fmt.Sprintf("customer-%d@e2e.test", time.Now().UnixNano()), "e2e-customer-password")
	s.awaitRoute(t, adminB, "/admin/products")
	s.awaitRoute(t, adminB, "/admin/zones")

	var product struct {
		ProductID string `json:"product_id"`
	}
	adminB.must(t, http.MethodPost, "/admin/products",
		map[string]any{"name": "B's kettle", "description": "Shop B only", "is_digital": false}, http.StatusCreated, &product)
	var zone struct {
		ZoneID string `json:"zone_id"`
	}
	adminB.must(t, http.MethodPost, "/admin/zones",
		map[string]any{"name": "B's zone", "priority": 90, "match_rules": []map[string]string{{"country_code": "ZA"}}},
		http.StatusCreated, &zone)

	t.Run("credentials work only on their own shop", func(t *testing.T) {
		for name, c := range map[string]*client{
			"customer":    customerA.as(shopB.host),
			"shop admin":  adminA.as(shopB.host),
			"system user": s.sys.as(shopB.host),
		} {
			if status, _ := c.do(t, http.MethodGet, "/me", nil, nil); status != http.StatusForbidden {
				t.Errorf("%s of another shop: GET /me = %d, want 403", name, status)
			}
		}
		if status, _ := adminA.as(s.sys.host).do(t, http.MethodGet, "/sysadmin/gdpr/register", nil, nil); status != http.StatusForbidden {
			t.Errorf("shop admin on the platform host = %d, want 403", status)
		}
	})

	t.Run("another shop's resources are invisible, even by ID", func(t *testing.T) {
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/products/" + product.ProductID, nil},
			{http.MethodPut, "/admin/products/" + product.ProductID, map[string]any{"name": "Taken", "description": "x", "is_digital": false}},
			{http.MethodDelete, "/admin/products/" + product.ProductID, nil},
			{http.MethodPut, "/admin/zones/" + zone.ZoneID, map[string]any{"name": "Taken", "priority": 1, "match_rules": []map[string]string{{"country_code": "ZA"}}}},
			{http.MethodDelete, "/admin/zones/" + zone.ZoneID, nil},
		} {
			if status, body := adminA.do(t, tc.method, tc.path, tc.body, nil); status != http.StatusNotFound {
				t.Errorf("shop A admin: %s %s = %d, want 404\n%s", tc.method, tc.path, status, body)
			}
		}
		for _, path := range []string{"/products", "/admin/zones"} {
			_, body := adminA.do(t, http.MethodGet, path, nil, nil)
			if strings.Contains(string(body), product.ProductID) || strings.Contains(string(body), zone.ZoneID) {
				t.Errorf("shop A's %s lists shop B's resources", path)
			}
		}
		for path, id := range map[string]string{"/products/" + product.ProductID: product.ProductID, "/admin/zones": zone.ZoneID} {
			if _, body := adminB.do(t, http.MethodGet, path, nil, nil); !strings.Contains(string(body), id) {
				t.Errorf("shop B cannot see its own %s at %s", id, path)
			}
		}
	})

	t.Run("each role reaches only its routes", func(t *testing.T) {
		visitor := s.client(shopA.host)
		for _, tc := range []struct {
			who    string
			c      *client
			method string
			path   string
			want   int
		}{
			{"visitor", visitor, http.MethodGet, "/orders/mine", http.StatusForbidden},
			{"visitor", visitor, http.MethodGet, "/admin/products", http.StatusUnauthorized},
			{"customer", customerA, http.MethodGet, "/admin/products", http.StatusForbidden},
			{"customer", customerA, http.MethodPost, "/admin/zones", http.StatusForbidden},
			{"customer", customerA, http.MethodGet, "/sysadmin/gdpr/register", http.StatusForbidden},
			{"shop admin", adminA, http.MethodGet, "/sysadmin/gdpr/register", http.StatusForbidden},
			{"shop admin", adminA, http.MethodGet, "/sysadmin/dlq/summary", http.StatusForbidden},
			{"anonymous", s.client(s.sys.host), http.MethodGet, "/sysadmin/gdpr/register", http.StatusUnauthorized},
		} {
			if status, _ := tc.c.do(t, tc.method, tc.path, nil, nil); status != tc.want {
				t.Errorf("%s: %s %s = %d, want %d", tc.who, tc.method, tc.path, status, tc.want)
			}
		}
	})

	t.Run("internal routes are not public", func(t *testing.T) {
		for _, path := range []string{"/internal/customer-orders?user_id=x", "/internal/skus/batch", "/internal/validate-token"} {
			for _, c := range []*client{customerA, adminA, s.sys} {
				if status, _ := c.do(t, http.MethodGet, path, nil, nil); status != http.StatusNotFound {
					t.Errorf("%s on %s = %d, want 404", path, c.host, status)
				}
			}
		}
	})

	t.Run("identity headers from clients are ignored", func(t *testing.T) {
		spoof := map[string]string{
			"X-YC-Tenant-ID":      tenantB,
			"X-YC-Actor-ID":       "00000000-0000-0000-0000-000000000000",
			"X-YC-Actor-Type":     "system_admin",
			"X-YC-Caller-Service": "yc-orders",
		}
		for path, want := range map[string]int{
			"/admin/products":                   http.StatusForbidden,
			"/sysadmin/gdpr/register":           http.StatusForbidden,
			"/internal/customer-orders?user_id": http.StatusNotFound,
		} {
			req, err := http.NewRequest(http.MethodGet, customerA.gateway+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range spoof {
				req.Header.Set(k, v)
			}
			resp := customerA.send(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("customer with spoofed headers: GET %s = %d, want %d", path, resp.StatusCode, want)
			}
		}
	})

	t.Run("odd paths do not bypass the admin audience", func(t *testing.T) {
		for _, path := range []string{
			"//admin/products",
			"/%61dmin/products",
			"/admin%2Fproducts",
			"/ADMIN/products",
			"/admin/../admin/products",
			"/products/../admin/products",
			"/admin/products/",
			"/admin/products;x=1",
		} {
			req, err := http.NewRequest(http.MethodGet, customerA.gateway+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp := customerA.send(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				t.Errorf("customer: GET %s = %d, want a refusal", path, resp.StatusCode)
			}
		}
	})

	t.Run("invalid credentials do not dodge the rate limits", func(t *testing.T) {
		// The visitor quota is 120 a minute with a burst of 30 per address;
		// each request carries a different invalid cookie, so without the
		// address limit each would get a fresh visitor and a fresh bucket.
		limited := false
		for i := range 60 {
			req, err := http.NewRequest(http.MethodGet, shopA.gateway+"/products", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = shopA.host
			req.AddCookie(&http.Cookie{Name: "yc_visitor", Value: fmt.Sprintf("forged-%d", i)})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if drain(resp) == http.StatusTooManyRequests {
				limited = resp.Header.Get("Retry-After") != ""
				break
			}
		}
		if !limited {
			t.Error("60 requests with forged visitor cookies were never refused with 429 and Retry-After")
		}
	})
}

// drain reads and closes the body and returns the status.
func drain(resp *http.Response) int {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}
