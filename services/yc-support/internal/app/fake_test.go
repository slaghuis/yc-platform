//go:build integration

package app_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// sent is a message yc-support asked yc-message to send.
type sent struct {
	Tenant string
	Body   map[string]any
}

func (s sent) typ() string             { return s.Body["type"].(string) }
func (s sent) ref() string             { return s.Body["reference"].(string) }
func (s sent) context() map[string]any { return s.Body["context"].(map[string]any) }
func (s sent) str(key string) string {
	v, _ := s.Body[key].(string)
	return v
}

// fakeGateway is yc-gateway's internal listener in front of yc-message's
// send API and yc-tenant's shop lookup.
type fakeGateway struct {
	mu   sync.Mutex
	sent []sent
	// failSend makes /internal/send answer 400, as yc-message does for a
	// type it has no template for.
	failSend bool
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/service-token" {
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "svc-yc-support", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	tenant := r.Header.Get(authctx.HeaderTenantID)
	if r.Header.Get("Authorization") != "Bearer svc-yc-support" || tenant == "" {
		problem(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	switch {
	case r.URL.Path == "/internal/send" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		fail := f.failSend
		if !fail {
			f.sent = append(f.sent, sent{Tenant: tenant, Body: body})
		}
		f.mu.Unlock()
		if fail {
			problem(w, http.StatusBadRequest, "validation_failed", "unknown message type")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"message_id": "8a6e0804-2bd0-4c3e-9a2f-8a0b8c1d2e3f"})
	case strings.HasPrefix(r.URL.Path, "/internal/tenants/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/internal/tenants/")
		writeJSON(w, http.StatusOK, map[string]any{"tenant_id": id, "name": id, "contact_email": "support@" + id + ".test"})
	default:
		problem(w, http.StatusNotFound, "not_found", "")
	}
}

// messages returns what was sent to yc-message so far.
func (f *fakeGateway) messages() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

// reset forgets what was sent and returns it.
func (f *fakeGateway) reset() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

func (f *fakeGateway) setFailSend(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSend = v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": code, "status": status, "code": code, "detail": detail,
	})
}
