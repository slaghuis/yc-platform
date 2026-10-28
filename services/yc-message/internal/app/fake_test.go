//go:build integration

package app_test

import (
	"bufio"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// mailbox is a fake SMTP server. Recipients in reject get a permanent 550;
// recipients in tempfail get a 451 that many times first.
type mailbox struct {
	ln       net.Listener
	mu       sync.Mutex
	mails    []sentMail
	reject   map[string]bool
	tempfail map[string]int
}

type sentMail struct {
	To, Subject, Body string
}

func newMailbox(t *testing.T) *mailbox {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox{ln: ln, reject: map[string]bool{}, tempfail: map[string]int{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	return m
}

func (m *mailbox) Addr() string { return m.ln.Addr().String() }

func (m *mailbox) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	reply := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	reply("220 fake ESMTP")
	var to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM"), cmd == "RSET", cmd == "NOOP":
			reply("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO"):
			to = strings.Trim(strings.TrimSpace(line)[len("RCPT TO:"):], "<> ")
			m.mu.Lock()
			rejected, temp := m.reject[to], m.tempfail[to] > 0
			if temp {
				m.tempfail[to]--
			}
			m.mu.Unlock()
			switch {
			case rejected:
				reply("550 no such user")
			case temp:
				reply("451 try again later")
			default:
				reply("250 OK")
			}
		case cmd == "DATA":
			reply("354 go ahead")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			m.store(to, data.String())
			reply("250 queued")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func (m *mailbox) store(to, raw string) {
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mails = append(m.mails, sentMail{To: to, Subject: subject, Body: strings.ReplaceAll(string(body), "\r\n", "\n")})
}

// to returns the mails sent to an address.
func (m *mailbox) to(addr string) []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentMail
	for _, ml := range m.mails {
		if ml.To == addr {
			out = append(out, ml)
		}
	}
	return out
}

// upstreams is yc-gateway's internal listener, serving yc-preference's and
// yc-orders' internal reads.
type upstreams struct {
	mu          sync.Mutex
	preferences map[string]map[string]any
	orders      map[string]map[string]any
	down        bool
}

func newUpstreams() *upstreams {
	return &upstreams{preferences: map[string]map[string]any{}, orders: map[string]map[string]any{}}
}

func (u *upstreams) set(f func(u *upstreams)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(u)
}

func (u *upstreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/service-token" {
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "svc-yc-message", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer svc-yc-message" || r.Header.Get(authctx.HeaderTenantID) == "" {
		problem(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.down {
		problem(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	var (
		found map[string]any
		ok    bool
	)
	switch {
	case strings.HasPrefix(r.URL.Path, "/internal/preferences/"):
		found, ok = u.preferences[strings.TrimPrefix(r.URL.Path, "/internal/preferences/")]
	case strings.HasSuffix(r.URL.Path, "/guest-access") && strings.HasPrefix(r.URL.Path, "/internal/orders/"):
		writeJSON(w, http.StatusOK, map[string]any{
			"token": "test-guest-token", "expires_at": time.Now().Add(24 * time.Hour).UTC(),
		})
		return
	case strings.HasPrefix(r.URL.Path, "/internal/orders/"):
		found, ok = u.orders[strings.TrimPrefix(r.URL.Path, "/internal/orders/")]
	}
	if !ok {
		problem(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": code, "status": status, "code": code})
}
