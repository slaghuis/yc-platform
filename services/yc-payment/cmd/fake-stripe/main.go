// Command fake-stripe serves the in-memory Stripe fake (stripetest) on a
// fixed address, so the dev stack can take payments without a Stripe
// account (scripts/demo-phase4.sh). Point yc-payment at it with
// YC_PAYMENT_STRIPE_API_BASE. Any sk_test_ key is accepted.
//
// Besides the Stripe API it answers
// POST /_fake/payment_intents/{id}/complete_action?success=true|false,
// which finishes a 3-D Secure challenge as the customer would.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe/stripetest"
)

func main() {
	addr := flag.String("addr", ":12111", "listen address")
	flag.Parse()

	fake := stripetest.New()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_fake/payment_intents/{id}/complete_action", func(w http.ResponseWriter, r *http.Request) {
		fake.CompleteAction(r.PathValue("id"), r.URL.Query().Get("success") != "false")
		w.WriteHeader(http.StatusNoContent)
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
