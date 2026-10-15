//go:build integration

package app_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// fakeMessages is yc-gateway's internal listener in front of yc-message's
// verification API. It accepts code "123456" for any address.
type fakeMessages struct {
	mu     sync.Mutex
	starts []map[string]any
}

const goodCode = "123456"

func (f *fakeMessages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/service-token" {
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "svc-yc-preference", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer svc-yc-preference" || r.Header.Get(authctx.HeaderTenantID) == "" {
		problem(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := time.Now().UTC()
	switch r.URL.Path {
	case "/internal/channels/verify-start":
		f.mu.Lock()
		f.starts = append(f.starts, body)
		f.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"verification_id": "8a6e0804-2bd0-4c3e-9a2f-8a0b8c1d2e3f", "channel": body["channel"], "address": body["address"],
			"expires_at": now.Add(15 * time.Minute),
		})
	case "/internal/channels/verify-confirm":
		if body["code"] != goodCode {
			problem(w, http.StatusBadRequest, "validation_failed", "The code is not correct; 4 attempts left")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"verification_id": "8a6e0804-2bd0-4c3e-9a2f-8a0b8c1d2e3f", "channel": body["channel"], "address": body["address"],
			"expires_at": now.Add(15 * time.Minute), "confirmed_at": now,
		})
	default:
		problem(w, http.StatusNotFound, "not_found", "")
	}
}

func (f *fakeMessages) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
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
		"errors": []map[string]string{{"field": "code", "code": "invalid", "message": detail}},
	})
}
