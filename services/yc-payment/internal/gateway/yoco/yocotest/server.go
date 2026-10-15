// Package yocotest is an in-memory fake of the Yoco Checkout API surface
// the driver uses, for tests, CI and the dev stack. Checkouts are paid (or
// fail) through Pay, as a customer would on Yoco's page; the fake then
// delivers a signed payment.succeeded or payment.failed webhook to every
// endpoint the paying account registered.
package yocotest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/yoco"
)

// MinAmount is the smallest payment Yoco accepts (R2.00).
const MinAmount = 200

// Checkout is a fake checkout.
type Checkout struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Amount      int64          `json:"amount"`
	Currency    string         `json:"currency"`
	RedirectURL string         `json:"redirectUrl"`
	PaymentID   *string        `json:"paymentId"`
	SuccessURL  *string        `json:"successUrl"`
	CancelURL   *string        `json:"cancelUrl"`
	FailureURL  *string        `json:"failureUrl"`
	Metadata    map[string]any `json:"metadata"`
	ExternalID  *string        `json:"externalId"`
	MerchantID  string         `json:"merchantId"`
	Mode        string         `json:"processingMode"`
	key         string
	refunded    int64
}

// Webhook is a registered endpoint.
type Webhook struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Mode   string `json:"mode"`
	Secret string `json:"secret,omitempty"`
	key    string
}

// Event is a webhook event the fake sent, kept for redelivery.
type Event struct {
	ID   string
	Body []byte
	key  string
}

// Delivery is one webhook POST and its answer.
type Delivery struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

type stored struct {
	status int
	body   []byte
}

type refund struct {
	checkoutID string
	amount     int64
}

// Server is the fake. Use URL (or the address it is served on) as the
// driver's API base.
type Server struct {
	*httptest.Server

	// Base is the address customers reach the fake on, for redirectUrl;
	// URL when empty.
	Base string
	// RefundsPending makes new refunds stay pending until SettleRefund.
	RefundsPending bool

	mu        sync.Mutex
	run       string
	seq       int
	checkouts map[string]*Checkout
	refunds   map[string]refund
	webhooks  []*Webhook
	events    map[string]Event
	idem      map[string]stored
	failNext  int
	calls     map[string]int
	client    *http.Client
}

// NewServer starts a fake; it is closed when the test ends.
func NewServer(t interface{ Cleanup(func()) }) *Server {
	s := New()
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Close)
	return s
}

// New returns a fake that is not listening, to serve on an address of the
// caller's choosing (cmd/fake-yoco). IDs carry a per-fake prefix so a
// restarted fake does not reuse the IDs yc-payment already stored.
func New() *Server {
	return &Server{
		run:       strconv.FormatInt(time.Now().UnixNano(), 36),
		checkouts: map[string]*Checkout{},
		refunds:   map[string]refund{},
		events:    map[string]Event{},
		idem:      map[string]stored{},
		calls:     map[string]int{},
		client:    &http.Client{Timeout: 10 * time.Second, Transport: localhostTransport()},
	}
}

// localhostTransport sends *.localhost to the loopback address, as
// browsers and curl do, so webhooks can name a shop's dev host.
func localhostTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(host, ".localhost") {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return dial(ctx, network, addr)
	}
	return t
}

// ServeHTTP answers a Checkout API request, a fake control or the
// payment page.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/_fake/"):
		s.control(w, r)
	case strings.HasPrefix(r.URL.Path, "/pay/"):
		s.page(w, r)
	default:
		s.api(w, r)
	}
}

// FailNext makes the next n API requests fail with 500, as in an outage.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// Calls returns how many requests reached route's handler, e.g.
// "POST /api/checkouts" (idempotent replays included).
func (s *Server) Calls(route string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[route]
}

// Checkout returns a copy of a checkout.
func (s *Server) Checkout(id string) (Checkout, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok {
		return Checkout{}, false
	}
	return *c, true
}

// Register adds a webhook endpoint for the account with apiKey and returns
// its secret, as POST /api/webhooks does.
func (s *Server) Register(apiKey, name, url string) Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.register(apiKey, name, url)
}

func (s *Server) register(key, name, url string) Webhook {
	s.seq++
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	w := &Webhook{
		ID: fmt.Sprintf("sub_fake_%s_%d", s.run, s.seq), Name: name, URL: url, Mode: "test",
		Secret: "whsec_" + base64.StdEncoding.EncodeToString(raw), key: key,
	}
	s.webhooks = append(s.webhooks, w)
	return *w
}

// Pay completes (or fails) a checkout as the customer would and delivers
// the webhook. It returns the event so tests can deliver it themselves.
func (s *Server) Pay(ctx context.Context, id string, success bool) (Event, []Delivery, error) {
	s.mu.Lock()
	c := s.checkouts[id]
	if c == nil {
		s.mu.Unlock()
		return Event{}, nil, fmt.Errorf("no checkout %s", id)
	}
	if c.Status == "completed" {
		s.mu.Unlock()
		return Event{}, nil, fmt.Errorf("checkout %s is already paid", id)
	}
	s.seq++
	paymentID := fmt.Sprintf("p_fake_%s_%d", s.run, s.seq)
	typ, status := "payment.failed", "failed"
	if success {
		c.Status, c.PaymentID = "completed", &paymentID
		typ, status = "payment.succeeded", "succeeded"
	} else {
		c.Status = "started"
	}
	ev := s.event(c.key, typ, map[string]any{
		"id": paymentID, "type": "payment", "createdDate": time.Now().UTC().Format(time.RFC3339),
		"amount": c.Amount, "currency": c.Currency, "status": status, "mode": "test",
		"metadata": merge(c.Metadata, map[string]any{"checkoutId": c.ID}),
		"paymentMethodDetails": map[string]any{"type": "card", "card": map[string]any{
			"expiryMonth": 12, "expiryYear": 2030, "maskedCard": "************1111", "scheme": "visa",
		}},
	})
	s.mu.Unlock()
	return ev, s.deliver(ctx, ev), nil
}

// SettleRefund finishes a pending refund and delivers its webhook.
func (s *Server) SettleRefund(ctx context.Context, refundID string, success bool) []Delivery {
	s.mu.Lock()
	rf := s.refunds[refundID]
	c := s.checkouts[rf.checkoutID]
	if c == nil {
		s.mu.Unlock()
		return nil
	}
	if !success {
		c.refunded -= rf.amount
	}
	ev := s.refundEvent(c, refundID, rf.amount, success)
	s.mu.Unlock()
	return s.deliver(ctx, ev)
}

// Redeliver sends an event again with the same ID, as Yoco retries.
func (s *Server) Redeliver(ctx context.Context, eventID string) ([]Delivery, bool) {
	s.mu.Lock()
	ev, ok := s.events[eventID]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return s.deliver(ctx, ev), true
}

func (s *Server) refundEvent(c *Checkout, refundID string, amount int64, success bool) Event {
	typ, status := "refund.failed", "failed"
	if success {
		typ, status = "refund.succeeded", "succeeded"
	}
	payload := map[string]any{
		"id": refundID, "type": "refund", "createdDate": time.Now().UTC().Format(time.RFC3339),
		"amount": amount, "currency": c.Currency, "status": status, "mode": "test",
		"metadata": map[string]any{"checkoutId": c.ID}, "refundableAmount": c.Amount - c.refunded,
	}
	if !success {
		payload["failureReason"] = "refund_declined"
	}
	return s.event(c.key, typ, payload)
}

func (s *Server) event(key, typ string, payload map[string]any) Event {
	s.seq++
	id := fmt.Sprintf("evt_fake_%s_%d", s.run, s.seq)
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": typ, "createdDate": time.Now().UTC().Format(time.RFC3339), "payload": payload,
	})
	ev := Event{ID: id, Body: body, key: key}
	s.events[id] = ev
	return ev
}

// Headers signs an event for secret, as Yoco sends it now.
func Headers(secret string, ev Event) http.Header {
	now := time.Now()
	sig, _ := yoco.Sign(secret, ev.ID, now, ev.Body)
	h := http.Header{}
	h.Set(yoco.IDHeader, ev.ID)
	h.Set(yoco.TimestampHeader, strconv.FormatInt(now.Unix(), 10))
	h.Set(yoco.SignatureHeader, sig)
	h.Set("Content-Type", "application/json")
	return h
}

func (s *Server) deliver(ctx context.Context, ev Event) []Delivery {
	s.mu.Lock()
	var targets []Webhook
	for _, w := range s.webhooks {
		if w.key == ev.key {
			targets = append(targets, *w)
		}
	}
	s.mu.Unlock()
	out := make([]Delivery, 0, len(targets))
	for _, w := range targets {
		d := Delivery{URL: w.URL}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(ev.Body))
		if err == nil {
			req.Header = Headers(w.Secret, ev)
			var resp *http.Response
			if resp, err = s.client.Do(req); err == nil {
				d.Status = resp.StatusCode
				_ = resp.Body.Close()
			}
		}
		if err != nil {
			d.Error = err.Error()
		}
		out = append(out, d)
	}
	return out
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		writeJSON(w, http.StatusInternalServerError, apiError("Internal error", "fake outage"))
		return
	}
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(key, "sk_test_") {
		writeJSON(w, http.StatusForbidden, apiError("A key is required, but has not been specified.", "unauthorized"))
		return
	}
	route := r.Method + " " + r.URL.Path
	if id, ok := refundPath(r.URL.Path); ok {
		route = r.Method + " /api/checkouts/{id}/refund"
		r.SetPathValue("id", id)
	}
	s.calls[route]++

	var in map[string]any
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON body", "invalid_input"))
			return
		}
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" && r.Method == http.MethodPost {
		if prev, ok := s.idem[key+"|"+route+"|"+idemKey]; ok {
			w.Header().Set("Idempotent-Replayed", "true")
			writeRaw(w, prev.status, prev.body)
			return
		}
	}
	status, body := s.handle(route, key, r, in)
	raw, _ := json.Marshal(body)
	if idemKey != "" && r.Method == http.MethodPost && status < 500 {
		s.idem[key+"|"+route+"|"+idemKey] = stored{status, raw}
	}
	writeRaw(w, status, raw)
}

func refundPath(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "/api/checkouts/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/refund")
	return id, ok && id != ""
}

func (s *Server) handle(route, key string, r *http.Request, in map[string]any) (int, any) {
	switch route {
	case "POST /api/checkouts":
		return s.create(key, r, in)
	case "POST /api/checkouts/{id}/refund":
		return s.refund(key, r.PathValue("id"), in)
	case "POST /api/webhooks":
		name, _ := in["name"].(string)
		url, _ := in["url"].(string)
		if name == "" || url == "" {
			return http.StatusBadRequest, apiError("name and url are required", "invalid_input")
		}
		return http.StatusCreated, s.register(key, name, url)
	case "GET /api/webhooks":
		out := []Webhook{}
		for _, w := range s.webhooks {
			if w.key == key {
				x := *w
				x.Secret = ""
				out = append(out, x)
			}
		}
		return http.StatusOK, map[string]any{"subscriptions": out}
	}
	return http.StatusNotFound, apiError("Not found", "not_found")
}

func (s *Server) create(key string, r *http.Request, in map[string]any) (int, any) {
	amount, ok := in["amount"].(float64)
	if !ok || amount != float64(int64(amount)) || int64(amount) < MinAmount {
		return http.StatusBadRequest, apiError("amount must be at least 200 cents", "invalid_input")
	}
	if in["currency"] != "ZAR" {
		return http.StatusBadRequest, apiError("Currently we only support ZAR", "invalid_input")
	}
	s.seq++
	c := &Checkout{
		ID: fmt.Sprintf("ch_fake_%s_%d", s.run, s.seq), Status: "created", Amount: int64(amount), Currency: "ZAR",
		SuccessURL: str(in["successUrl"]), CancelURL: str(in["cancelUrl"]), FailureURL: str(in["failureUrl"]),
		ExternalID: str(in["externalId"]), MerchantID: "fake-merchant", Mode: "test", key: key,
	}
	if m, ok := in["metadata"].(map[string]any); ok {
		c.Metadata = m
	}
	base := s.Base
	if base == "" {
		base = "http://" + r.Host
	}
	c.RedirectURL = strings.TrimRight(base, "/") + "/pay/" + c.ID
	s.checkouts[c.ID] = c
	return http.StatusOK, c
}

func (s *Server) refund(key, id string, in map[string]any) (int, any) {
	c := s.checkouts[id]
	if c == nil || c.key != key {
		return http.StatusNotFound, apiError("Checkout not found", "not_found")
	}
	if c.Status != "completed" {
		return http.StatusBadRequest, apiError("The checkout has not been paid", "checkout_not_paid")
	}
	amount := c.Amount - c.refunded
	if v, ok := in["amount"].(float64); ok {
		amount = int64(v)
	}
	if amount <= 0 || amount > c.Amount-c.refunded {
		return http.StatusBadRequest, apiError("Refund amount exceeds the refundable amount", "amount_too_large")
	}
	s.seq++
	refundID := fmt.Sprintf("rf_fake_%s_%d", s.run, s.seq)
	c.refunded += amount
	s.refunds[refundID] = refund{checkoutID: c.ID, amount: amount}
	status := "succeeded"
	if s.RefundsPending {
		status = "pending"
	}
	return http.StatusOK, map[string]any{"id": c.ID, "refundId": refundID, "message": "Refund " + status, "status": status}
}

// control serves the fake's own endpoints:
//
//	POST /_fake/checkouts/{id}/pay?success=true|false
//	POST /_fake/refunds/{id}/settle?success=true|false
//	POST /_fake/events/{id}/redeliver
//	POST /_fake/webhooks {api_key, name, url}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("POST only", "method_not_allowed"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/_fake/"), "/")
	success := r.URL.Query().Get("success") != "false"
	switch {
	case len(parts) == 3 && parts[0] == "checkouts" && parts[2] == "pay":
		ev, deliveries, err := s.Pay(r.Context(), parts[1], success)
		if err != nil {
			writeJSON(w, http.StatusConflict, apiError(err.Error(), "invalid_state"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"event_id": ev.ID, "deliveries": deliveries})
	case len(parts) == 3 && parts[0] == "refunds" && parts[2] == "settle":
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": s.SettleRefund(r.Context(), parts[1], success)})
	case len(parts) == 3 && parts[0] == "events" && parts[2] == "redeliver":
		deliveries, ok := s.Redeliver(r.Context(), parts[1])
		if !ok {
			writeJSON(w, http.StatusNotFound, apiError("No such event", "not_found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"event_id": parts[1], "deliveries": deliveries})
	case len(parts) == 1 && parts[0] == "webhooks":
		var in struct {
			APIKey string `json:"api_key"`
			Name   string `json:"name"`
			URL    string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.APIKey == "" || in.URL == "" {
			writeJSON(w, http.StatusBadRequest, apiError("api_key and url are required", "invalid_input"))
			return
		}
		writeJSON(w, http.StatusCreated, s.Register(in.APIKey, in.Name, in.URL))
	default:
		writeJSON(w, http.StatusNotFound, apiError("Not found", "not_found"))
	}
}

// page is the stand-in for Yoco's payment page.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Checkout(strings.TrimPrefix(r.URL.Path, "/pay/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := html.EscapeString(c.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><title>Fake Yoco</title>
<h1>Fake Yoco checkout</h1><p>%s: R%d.%02d, %s</p>
<form method="post" action="/_fake/checkouts/%s/pay?success=true"><button>Pay</button></form>
<form method="post" action="/_fake/checkouts/%s/pay?success=false"><button>Decline</button></form>`,
		id, c.Amount/100, c.Amount%100, html.EscapeString(c.Status), id, id)
}

func str(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

func apiError(msg, reason string) map[string]any {
	return map[string]any{"message": msg, "reason": reason, "solution": "See the fake's package documentation."}
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
