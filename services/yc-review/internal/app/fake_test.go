//go:build integration

package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

type order struct {
	OrderID     string      `json:"order_id"`
	OrderNumber string      `json:"order_number"`
	UserID      string      `json:"user_id"`
	UserType    string      `json:"user_type"`
	Items       []orderItem `json:"items"`
}

type orderItem struct {
	ProductID    string `json:"product_id"`
	NameSnapshot string `json:"name_snapshot"`
}

// ordersGateway is yc-gateway in front of yc-orders: it issues yc-review's
// service token and answers purchase and order lookups for shop-a.
type ordersGateway struct {
	*httptest.Server

	mu        sync.Mutex
	down      bool
	purchases map[string]string // user|product → order ID
	orders    map[string]order
}

func newOrdersGateway(t *testing.T) *ordersGateway {
	t.Helper()
	g := &ordersGateway{purchases: map[string]string{}, orders: map[string]order{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *ordersGateway) setDown(down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = down
}

func (g *ordersGateway) addOrder(o order) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orders[o.OrderID] = o
	for _, it := range o.Items {
		g.purchases[o.UserID+"|"+it.ProductID] = o.OrderID
	}
}

func (g *ordersGateway) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/internal/service-token" {
		_, _ = w.Write([]byte(`{"access_token":"svc-yc-review","expires_at":"2099-01-01T00:00:00Z"}`))
		return
	}
	if r.Header.Get(authctx.HeaderAuthorization) != "Bearer svc-yc-review" || r.Header.Get(authctx.HeaderTenantID) != "shop-a" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/internal/purchases":
		q := r.URL.Query()
		id, ok := g.purchases[q.Get("user_id")+"|"+q.Get("product_id")]
		out := map[string]any{"purchased": ok}
		if ok {
			out["order_id"] = id
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, "/internal/orders/"):
		o, ok := g.orders[strings.TrimPrefix(r.URL.Path, "/internal/orders/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
