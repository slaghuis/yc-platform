// Package stripetest is an in-memory fake of the Stripe API surface the
// driver uses, for tests and CI. It honours Stripe's test payment methods:
//
//   - pm_card_visa (and any other) is authorised;
//   - pm_card_chargeDeclined is declined (402 card_declined);
//   - pm_card_threeDSecure2Required needs 3-D Secure (requires_action);
//     CompleteAction finishes it.
package stripetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe"
)

// Test payment methods.
const (
	CardVisa     = "pm_card_visa"
	CardDeclined = "pm_card_chargeDeclined"
	Card3DS      = "pm_card_threeDSecure2Required"
)

// Intent is a fake PaymentIntent.
type Intent struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	Amount         int64             `json:"amount"`
	AmountReceived int64             `json:"amount_received"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	CaptureMethod  string            `json:"capture_method"`
	PaymentMethod  string            `json:"payment_method"`
	Metadata       map[string]string `json:"metadata"`
	NextAction     map[string]any    `json:"next_action"`
	LastError      map[string]any    `json:"last_payment_error"`
	refunded       int64
}

// Refund is a fake refund.
type Refund struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Amount        int64  `json:"amount"`
	Status        string `json:"status"`
	PaymentIntent string `json:"payment_intent"`
}

type stored struct {
	status int
	body   []byte
}

// Server is the fake. Use URL as the driver's API base.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	run      string
	seq      int
	intents  map[string]*Intent
	refunds  map[string]*Refund
	idem     map[string]stored
	failNext int
	calls    map[string]int

	// RefundsPending makes new refunds stay pending until SettleRefund.
	RefundsPending bool
}

// NewServer starts a fake; it is closed when the test ends.
func NewServer(t interface{ Cleanup(func()) }) *Server {
	s := New()
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Close)
	return s
}

// New returns a fake that is not listening, to serve on an address of
// the caller's choosing (cmd/fake-stripe). URL and Close are unset.
// IDs carry a per-fake prefix so a restarted fake does not reuse the IDs
// yc-payment already stored.
func New() *Server {
	return &Server{
		run:     strconv.FormatInt(time.Now().UnixNano(), 36),
		intents: map[string]*Intent{},
		refunds: map[string]*Refund{},
		idem:    map[string]stored{},
		calls:   map[string]int{},
	}
}

// ServeHTTP answers a Stripe API request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serve(w, r) }

// FailNext makes the next n requests fail with 500, as in an outage.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// Calls returns how many requests reached path's handler, e.g.
// "POST /v1/payment_intents" (idempotent replays included).
func (s *Server) Calls(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[route]
}

// Intent returns a copy of a PaymentIntent.
func (s *Server) Intent(id string) (Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi, ok := s.intents[id]
	if !ok {
		return Intent{}, false
	}
	return *pi, true
}

// CompleteAction finishes a 3-D Secure challenge, successfully or not.
func (s *Server) CompleteAction(id string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi := s.intents[id]
	if pi == nil || pi.Status != "requires_action" {
		return
	}
	pi.NextAction = nil
	if success {
		pi.Status = "requires_capture"
		return
	}
	pi.Status = "requires_payment_method"
	pi.LastError = map[string]any{"type": "card_error", "code": "payment_intent_authentication_failure"}
}

// SettleRefund finishes a pending refund.
func (s *Server) SettleRefund(id string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.refunds[id]
	if r == nil {
		return
	}
	r.Status = "failed"
	if success {
		r.Status = "succeeded"
	}
}

// Webhook returns a signed event of eventType about object (an intent or
// refund ID) in its current state, and its Stripe-Signature header.
func (s *Server) Webhook(secret, eventType, object string) (body []byte, signature string) {
	s.mu.Lock()
	var obj any
	if pi, ok := s.intents[object]; ok {
		obj = pi
	} else if r, ok := s.refunds[object]; ok {
		obj = r
	}
	s.seq++
	id := fmt.Sprintf("evt_%s_%d", s.run, s.seq)
	body, _ = json.Marshal(map[string]any{
		"id": id, "object": "event", "type": eventType,
		"data": map[string]any{"object": obj},
	})
	s.mu.Unlock()
	return body, stripe.Sign(secret, body, time.Now())
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		writeJSON(w, http.StatusInternalServerError, apiError("api_error", "", "fake outage"))
		return
	}
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(key, "sk_test_") {
		writeJSON(w, http.StatusUnauthorized, apiError("invalid_request_error", "api_key_invalid", "Invalid API Key provided"))
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("invalid_request_error", "parameter_invalid", err.Error()))
		return
	}
	route := r.Method + " " + r.URL.Path
	if id, action, ok := intentPath(r.URL.Path); ok {
		route = r.Method + " /v1/payment_intents/{id}" + action
		r.SetPathValue("id", id)
	}
	s.calls[route]++

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" && r.Method == http.MethodPost {
		if prev, ok := s.idem[key+"|"+idemKey]; ok {
			w.Header().Set("Idempotent-Replayed", "true")
			writeRaw(w, prev.status, prev.body)
			return
		}
	}
	status, body := s.handle(route, r)
	raw, _ := json.Marshal(body)
	if idemKey != "" && r.Method == http.MethodPost && status < 500 {
		s.idem[key+"|"+idemKey] = stored{status, raw}
	}
	writeRaw(w, status, raw)
}

func intentPath(p string) (id, action string, ok bool) {
	rest, ok := strings.CutPrefix(p, "/v1/payment_intents/")
	if !ok || rest == "" {
		return "", "", false
	}
	id, action, _ = strings.Cut(rest, "/")
	if action != "" {
		action = "/" + action
	}
	return id, action, true
}

func (s *Server) handle(route string, r *http.Request) (int, any) {
	switch route {
	case "POST /v1/payment_intents":
		return s.create(r)
	case "GET /v1/payment_intents/{id}":
		if pi := s.intents[r.PathValue("id")]; pi != nil {
			return http.StatusOK, pi
		}
	case "POST /v1/payment_intents/{id}/capture":
		if pi := s.intents[r.PathValue("id")]; pi != nil {
			return s.capture(pi, r)
		}
	case "POST /v1/payment_intents/{id}/cancel":
		if pi := s.intents[r.PathValue("id")]; pi != nil {
			return s.cancel(pi)
		}
	case "POST /v1/refunds":
		return s.refund(r)
	case "GET /v1/balance":
		return http.StatusOK, map[string]any{"object": "balance", "available": []any{}}
	default:
		return http.StatusNotFound, apiError("invalid_request_error", "resource_missing", "Unknown request URL")
	}
	return http.StatusNotFound, apiError("invalid_request_error", "resource_missing", "No such payment_intent")
}

func (s *Server) create(r *http.Request) (int, any) {
	amount, err := strconv.ParseInt(r.PostForm.Get("amount"), 10, 64)
	if err != nil || amount <= 0 {
		return http.StatusBadRequest, apiError("invalid_request_error", "parameter_invalid_integer", "Invalid amount")
	}
	if r.PostForm.Get("currency") == "" || r.PostForm.Get("payment_method") == "" {
		return http.StatusBadRequest, apiError("invalid_request_error", "parameter_missing", "Missing currency or payment_method")
	}
	s.seq++
	pi := &Intent{
		ID: fmt.Sprintf("pi_fake_%s_%d", s.run, s.seq), Object: "payment_intent",
		Amount: amount, Currency: r.PostForm.Get("currency"),
		CaptureMethod: r.PostForm.Get("capture_method"),
		PaymentMethod: r.PostForm.Get("payment_method"),
		Metadata:      map[string]string{},
	}
	for k, v := range r.PostForm {
		if m, ok := strings.CutPrefix(k, "metadata["); ok {
			pi.Metadata[strings.TrimSuffix(m, "]")] = v[0]
		}
	}
	s.intents[pi.ID] = pi
	switch pi.PaymentMethod {
	case CardDeclined:
		pi.Status = "requires_payment_method"
		pi.LastError = map[string]any{"type": "card_error", "code": "card_declined", "decline_code": "generic_decline"}
		e := apiError("card_error", "card_declined", "Your card was declined.")
		e["error"].(map[string]any)["decline_code"] = "generic_decline"
		e["error"].(map[string]any)["payment_intent"] = pi
		return http.StatusPaymentRequired, e
	case Card3DS:
		pi.Status = "requires_action"
		pi.NextAction = map[string]any{"type": "use_stripe_sdk", "use_stripe_sdk": map[string]any{"type": "three_d_secure_redirect"}}
	default:
		pi.Status = "requires_capture"
		if pi.CaptureMethod != "manual" {
			pi.Status = "succeeded"
			pi.AmountReceived = amount
		}
	}
	return http.StatusOK, pi
}

func (s *Server) capture(pi *Intent, r *http.Request) (int, any) {
	if pi.Status != "requires_capture" {
		return http.StatusBadRequest, unexpectedState(pi)
	}
	amount := pi.Amount
	if v := r.PostForm.Get("amount_to_capture"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > pi.Amount {
			return http.StatusBadRequest, apiError("invalid_request_error", "parameter_invalid_integer", "Invalid amount_to_capture")
		}
		amount = n
	}
	pi.Status = "succeeded"
	pi.AmountReceived = amount
	return http.StatusOK, pi
}

func (s *Server) cancel(pi *Intent) (int, any) {
	switch pi.Status {
	case "requires_capture", "requires_action", "requires_payment_method", "requires_confirmation":
		pi.Status = "canceled"
		pi.NextAction = nil
		return http.StatusOK, pi
	default:
		return http.StatusBadRequest, unexpectedState(pi)
	}
}

func (s *Server) refund(r *http.Request) (int, any) {
	pi := s.intents[r.PostForm.Get("payment_intent")]
	if pi == nil {
		return http.StatusNotFound, apiError("invalid_request_error", "resource_missing", "No such payment_intent")
	}
	if pi.Status != "succeeded" {
		return http.StatusBadRequest, apiError("invalid_request_error", "charge_not_captured", "This PaymentIntent has not been captured")
	}
	amount := pi.AmountReceived - pi.refunded
	if v := r.PostForm.Get("amount"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return http.StatusBadRequest, apiError("invalid_request_error", "parameter_invalid_integer", "Invalid amount")
		}
		amount = n
	}
	if amount <= 0 || amount > pi.AmountReceived-pi.refunded {
		return http.StatusBadRequest, apiError("invalid_request_error", "amount_too_large", "Refund amount exceeds the remaining charge")
	}
	s.seq++
	ref := &Refund{ID: fmt.Sprintf("re_fake_%s_%d", s.run, s.seq), Object: "refund", Amount: amount, PaymentIntent: pi.ID, Status: "succeeded"}
	if s.RefundsPending {
		ref.Status = "pending"
	}
	pi.refunded += amount
	s.refunds[ref.ID] = ref
	return http.StatusOK, ref
}

func unexpectedState(pi *Intent) map[string]any {
	e := apiError("invalid_request_error", "payment_intent_unexpected_state",
		"This PaymentIntent's status is "+pi.Status)
	e["error"].(map[string]any)["payment_intent"] = pi
	return e
}

func apiError(typ, code, msg string) map[string]any {
	e := map[string]any{"type": typ, "message": msg}
	if code != "" {
		e["code"] = code
	}
	return map[string]any{"error": e}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	writeRaw(w, status, raw)
}

func writeRaw(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
