// Command fake-stripe serves the in-memory Stripe fake (stripetest) on a
// fixed address, so the dev stack can take payments without a Stripe
// account (scripts/demo-phase4.sh). Point yc-payment at it with
// YC_PAYMENT_STRIPE_API_BASE. Any sk_test_ key is accepted.
//
// Besides the Stripe API it answers
//
//   - POST /_fake/payment_intents/{id}/complete_action?success=true|false,
//     which finishes a 3-D Secure challenge as the customer would;
//   - POST /_fake/payment_intents/{id}/dispute with
//     {"webhook_url", "webhook_secret", "reason"}, which opens a dispute
//     (chargeback) on a captured payment and sends the signed
//     charge.dispute.created webhook (scripts/demo-phase8.sh).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe/stripetest"
)

func main() {
	addr := flag.String("addr", ":12111", "listen address")
	flag.Parse()

	fake := stripetest.New()
	client := &http.Client{Timeout: 10 * time.Second, Transport: localhostTransport()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_fake/payment_intents/{id}/complete_action", func(w http.ResponseWriter, r *http.Request) {
		fake.CompleteAction(r.PathValue("id"), r.URL.Query().Get("success") != "false")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/payment_intents/{id}/dispute", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			WebhookURL    string `json:"webhook_url"`
			WebhookSecret string `json:"webhook_secret"`
			Reason        string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.WebhookURL == "" {
			http.Error(w, "webhook_url is required", http.StatusBadRequest)
			return
		}
		if in.Reason == "" {
			in.Reason = "fraudulent"
		}
		body, sig, disputeID, ok := fake.Dispute(in.WebhookSecret, r.PathValue("id"), in.Reason)
		if !ok {
			http.Error(w, "no captured payment_intent "+r.PathValue("id"), http.StatusNotFound)
			return
		}
		out := map[string]any{"dispute_id": disputeID}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, in.WebhookURL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(stripe.SignatureHeader, sig)
			var resp *http.Response
			if resp, err = client.Do(req); err == nil { //nolint:gosec // a dev-only fake posting where its caller asks
				out["status"] = resp.StatusCode
				_ = resp.Body.Close()
			}
		}
		if err != nil {
			out["error"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.Handle("/", fake)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown) //nolint:contextcheck // the signal context is already done
	}()

	slog.Info("fake Stripe listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "fake-stripe:", err)
		os.Exit(1)
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
