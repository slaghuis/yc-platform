//go:build integration

package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Payment method tokens the fake understands, as in Stripe test mode.
const (
	cardVisa     = "pm_card_visa"
	cardDeclined = "pm_card_chargeDeclined"
	card3DS      = "pm_card_threeDSecure2Required"
)

type fakeSKU struct {
	ID       string
	Name     string
	Digital  bool
	Tracked  bool
	Price    int64 // gross per unit, VAT-inclusive at 15 %
	Stock    int64
	Inactive bool
}

type fakeLine struct {
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
}

type fakeReservation struct {
	ID     string
	Status string
	Items  []fakeLine
}

type fakeTx struct {
	ID        string
	SessionID string
	Status    string
	Token     string
	Reason    string
}

type fakeOrder struct {
	ID        string
	SessionID string
	Status    string
	Number    string
	Body      map[string]any
}

// fakeGateway stands in for yc-gateway's internal listener in front of
// yc-cart, yc-product, yc-pricing, yc-inventory, yc-payment and yc-orders.
type fakeGateway struct {
	mu           sync.Mutex
	carts        map[string][]fakeLine
	skus         map[string]*fakeSKU
	reservations map[string]*fakeReservation
	reserveKeys  map[string]string
	txs          map[string]*fakeTx
	authKeys     map[string]string
	orders       map[string]*fakeOrder
	orderSeq     int
	calls        []string
	// fail answers 503 to the next n calls of a step.
	fail map[string]int
	// declineCapture answers capture with 400 payment_declined.
	declineCapture bool
	// loseCapture captures but answers 503, as if the reply was lost.
	loseCapture bool
	// actions completes 3-D Secure per session: true succeeds.
	actions map[string]bool
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		carts: map[string][]fakeLine{}, skus: map[string]*fakeSKU{}, reservations: map[string]*fakeReservation{},
		reserveKeys: map[string]string{}, txs: map[string]*fakeTx{}, authKeys: map[string]string{},
		orders: map[string]*fakeOrder{}, fail: map[string]int{}, actions: map[string]bool{},
	}
}

func (f *fakeGateway) sku(s fakeSKU) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.ID = uuid.NewString()
	f.skus[s.ID] = &s
	return s.ID
}

func (f *fakeGateway) setCart(userID string, lines ...fakeLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.carts[userID] = lines
}

func (f *fakeGateway) stock(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.skus[id].Stock
}

func (f *fakeGateway) failNext(step string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[step] = n
}

func (f *fakeGateway) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// callsFor returns the steps called for a session, with their keys.
func (f *fakeGateway) callsFor(sessionID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, sessionID) {
			out = append(out, strings.ReplaceAll(c, sessionID, "S"))
		}
	}
	return out
}

func (f *fakeGateway) txFor(sessionID string) *fakeTx {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.txs {
		if t.SessionID == sessionID {
			c := *t
			return &c
		}
	}
	return nil
}

func (f *fakeGateway) orderFor(sessionID string) *fakeOrder {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orders {
		if o.SessionID == sessionID {
			c := *o
			return &c
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code, detail string, ext map[string]any) {
	p := map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "detail": detail}
	for k, v := range ext {
		p[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/service-token" {
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "svc-yc-checkout", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer svc-yc-checkout" || r.Header.Get(authctx.HeaderTenantID) == "" {
		problem(w, http.StatusUnauthorized, "unauthorized", "no service token", nil)
		return
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	key := r.Header.Get(httpkit.HeaderIdempotencyKey)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/internal/"), "/")
	step := parts[0]
	if len(parts) == 3 && step != "carts" {
		step = parts[0] + "." + parts[2]
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if key != "" {
		f.calls = append(f.calls, step+" "+key)
	}
	if f.fail[step] > 0 {
		f.fail[step]--
		problem(w, http.StatusServiceUnavailable, "upstream_failure", "injected outage", nil)
		return
	}
	switch {
	case step == "carts":
		lines := f.carts[parts[1]]
		if lines == nil {
			lines = []fakeLine{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"cart_id": uuid.NewString(), "user_id": parts[1], "cart_type": parts[2],
			"convertible": parts[2] == "shopping", "items": lines,
		})
	case step == "skus":
		var skus []map[string]any
		missing := []string{}
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			s, ok := f.skus[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			status := "active"
			if s.Inactive {
				status = "inactive"
			}
			skus = append(skus, map[string]any{
				"sku_id": s.ID, "product_id": uuid.NewString(), "sku_code": "SKU-" + s.Name, "product_name": s.Name,
				"variant_values": map[string]any{}, "is_digital": s.Digital, "inventory_tracked": s.Tracked,
				"tax_class": "standard", "status": status,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"skus": skus, "missing": missing})
	case step == "prices":
		f.evaluate(w, body)
	case step == "reservations" && len(parts) == 1:
		f.reserve(w, key, body)
	case step == "reservations.commit", step == "reservations.release":
		f.settleReservation(w, parts[1], strings.TrimPrefix(step, "reservations."))
	case step == "authorize":
		f.authorize(w, key, body)
	case step == "capture":
		f.capture(w, body)
	case step == "void":
		f.void(w, body)
	case step == "transactions":
		t, ok := f.txs[parts[1]]
		if !ok {
			problem(w, http.StatusNotFound, "not_found", "no transaction", nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"transaction_id": t.ID, "status": t.Status})
	case step == "orders" && len(parts) == 1:
		f.createOrder(w, body)
	case step == "orders.confirm", step == "orders.abandon":
		f.settleOrder(w, parts[1], strings.TrimPrefix(step, "orders."))
	default:
		problem(w, http.StatusNotFound, "not_found", r.URL.Path, nil)
	}
}

func lines(v any) []fakeLine {
	b, _ := json.Marshal(v)
	var out []fakeLine
	_ = json.Unmarshal(b, &out)
	return out
}

func zar(minor int64) map[string]any { return map[string]any{"amount_minor": minor, "currency": "ZAR"} }

func (f *fakeGateway) evaluate(w http.ResponseWriter, body map[string]any) {
	priced := make([]map[string]any, 0, len(lines(body["lines"])))
	var tax, total int64
	for _, l := range lines(body["lines"]) {
		gross := f.skus[l.SKUID].Price * l.Quantity
		t := (gross*15 + 57) / 115
		tax, total = tax+t, total+gross
		priced = append(priced, map[string]any{
			"sku_id": l.SKUID, "quantity": l.Quantity, "tax_class": "standard", "unit_price": zar(f.skus[l.SKUID].Price),
			"price_includes_tax": true, "net": zar(gross - t), "tax": zar(t), "gross": zar(gross), "taxes": []any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"store_currency": "ZAR", "tax_inclusive": true, "jurisdiction": "ZA", "lines": priced, "unpriced": []any{},
		"totals":      map[string]any{"subtotal_excl_tax": zar(total - tax), "tax_total": zar(tax), "grand_total": zar(total), "tax_breakdown": []any{}},
		"audit_trail": []any{}, "evaluated_at": time.Now(),
	})
}

func (f *fakeGateway) reserve(w http.ResponseWriter, key string, body map[string]any) {
	if id, ok := f.reserveKeys[key]; ok {
		writeJSON(w, http.StatusOK, map[string]any{"reservation_id": id, "status": f.reservations[id].Status, "items": f.reservations[id].Items})
		return
	}
	items := lines(body["items"])
	var shortages []map[string]any
	for _, l := range items {
		if s := f.skus[l.SKUID]; s.Stock < l.Quantity {
			shortages = append(shortages, map[string]any{"sku_id": l.SKUID, "requested": l.Quantity, "available": s.Stock})
		}
	}
	if len(shortages) > 0 {
		problem(w, http.StatusConflict, "insufficient_stock", "Not enough stock", map[string]any{"shortages": shortages})
		return
	}
	for _, l := range items {
		f.skus[l.SKUID].Stock -= l.Quantity
	}
	id := uuid.NewString()
	f.reservations[id] = &fakeReservation{ID: id, Status: "active", Items: items}
	f.reserveKeys[key] = id
	writeJSON(w, http.StatusOK, map[string]any{
		"reservation_id": id, "status": "active", "expires_at": time.Now().Add(15 * time.Minute), "items": items,
	})
}

func (f *fakeGateway) settleReservation(w http.ResponseWriter, id, action string) {
	res, ok := f.reservations[id]
	switch {
	case action == "release" && (!ok || res.Status != "active"):
		writeJSON(w, http.StatusOK, map[string]any{"reservation_id": id, "status": "unknown"})
	case action == "release":
		for _, l := range res.Items {
			f.skus[l.SKUID].Stock += l.Quantity
		}
		res.Status = "released"
		writeJSON(w, http.StatusOK, map[string]any{"reservation_id": id, "status": "released"})
	case !ok:
		problem(w, http.StatusNotFound, "not_found", "no reservation", nil)
	case res.Status == "released":
		problem(w, http.StatusConflict, "invalid_state_transition", "released", nil)
	default:
		res.Status = "committed"
		writeJSON(w, http.StatusOK, map[string]any{"reservation_id": id, "status": "committed", "items": res.Items})
	}
}

func (f *fakeGateway) txView(t *fakeTx) map[string]any {
	out := map[string]any{"transaction_id": t.ID, "session_id": t.SessionID, "status": t.Status}
	if t.Status == "pending" {
		out["next_action"] = map[string]any{"type": "use_stripe_sdk"}
	}
	if t.Reason != "" {
		out["failure_reason"] = t.Reason
	}
	return out
}

func (f *fakeGateway) authorize(w http.ResponseWriter, key string, body map[string]any) {
	if id, ok := f.authKeys[key]; ok {
		t := f.txs[id]
		if done, ok := f.actions[t.SessionID]; ok && t.Status == "pending" {
			t.Status = map[bool]string{true: "authorized", false: "failed"}[done]
			if !done {
				t.Reason = "authentication_failed"
			}
		}
		writeJSON(w, http.StatusOK, f.txView(t))
		return
	}
	token, _ := body["payment_token"].(string)
	t := &fakeTx{ID: uuid.NewString(), SessionID: fmt.Sprint(body["session_id"]), Token: token}
	switch token {
	case cardVisa:
		t.Status = "authorized"
	case card3DS:
		t.Status = "pending"
	default:
		t.Status, t.Reason = "failed", "card_declined"
	}
	f.txs[t.ID], f.authKeys[key] = t, t.ID
	writeJSON(w, http.StatusOK, f.txView(t))
}

func (f *fakeGateway) capture(w http.ResponseWriter, body map[string]any) {
	t, ok := f.txs[fmt.Sprint(body["transaction_id"])]
	switch {
	case !ok:
		problem(w, http.StatusNotFound, "not_found", "no transaction", nil)
	case f.declineCapture:
		t.Status = "failed"
		problem(w, http.StatusBadRequest, "payment_declined", "Capture refused", map[string]any{"reason": "expired_card"})
	case t.Status == "authorized" || t.Status == "captured":
		t.Status = "captured"
		if f.loseCapture {
			problem(w, http.StatusServiceUnavailable, "gateway_unavailable", "reply lost", nil)
			return
		}
		writeJSON(w, http.StatusOK, f.txView(t))
	default:
		problem(w, http.StatusConflict, "invalid_state_transition", "not authorized", nil)
	}
}

func (f *fakeGateway) void(w http.ResponseWriter, body map[string]any) {
	id, _ := body["transaction_id"].(string)
	if id == "" {
		id = f.authKeys[fmt.Sprint(body["authorization_key"])]
	}
	t, ok := f.txs[id]
	switch {
	case !ok:
		writeJSON(w, http.StatusOK, map[string]any{"status": "none"})
	case t.Status == "captured":
		problem(w, http.StatusConflict, "invalid_state_transition", "captured", map[string]any{"transaction_id": t.ID})
	case t.Status == "failed":
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "transaction_id": t.ID})
	default:
		t.Status = "voided"
		writeJSON(w, http.StatusOK, map[string]any{"status": "voided", "transaction_id": t.ID})
	}
}

func (f *fakeGateway) createOrder(w http.ResponseWriter, body map[string]any) {
	session := fmt.Sprint(body["checkout_session_id"])
	for _, o := range f.orders {
		if o.SessionID == session {
			writeJSON(w, http.StatusCreated, map[string]any{"order_id": o.ID, "status": o.Status})
			return
		}
	}
	if body["user_type"] == "visitor" && body["guest_contact"] == nil {
		problem(w, http.StatusBadRequest, "validation_failed", "guest contact", nil)
		return
	}
	o := &fakeOrder{ID: uuid.NewString(), SessionID: session, Status: "pending_payment", Body: body}
	f.orders[o.ID] = o
	writeJSON(w, http.StatusCreated, map[string]any{"order_id": o.ID, "status": o.Status})
}

func (f *fakeGateway) settleOrder(w http.ResponseWriter, id, action string) {
	o, ok := f.orders[id]
	switch {
	case !ok && action == "abandon":
		writeJSON(w, http.StatusOK, map[string]any{"status": "none"})
	case !ok:
		problem(w, http.StatusNotFound, "not_found", "no order", nil)
	case action == "abandon":
		o.Status = "abandoned"
		writeJSON(w, http.StatusOK, map[string]any{"order_id": id, "status": "abandoned"})
	default:
		if o.Number == "" {
			f.orderSeq++
			o.Number, o.Status = fmt.Sprintf("SHOP-A-%06d", f.orderSeq), "received"
		}
		writeJSON(w, http.StatusOK, map[string]any{"order_id": id, "order_number": o.Number, "status": o.Status})
	}
}
