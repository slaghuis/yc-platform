// Command fake-yoco serves the in-memory Yoco fake (yocotest) on a fixed
// address, so the dev stack can take Yoco payments without a Yoco account
// (scripts/demo-phase5.sh). Point yc-payment at it with
// YC_PAYMENT_YOCO_API_BASE. Any sk_test_ key is accepted.
//
// Besides the Checkout API (checkouts, refunds, webhook registration) it
// serves the payment page at /pay/{id} and answers
//
//	POST /_fake/checkouts/{id}/pay?success=true|false
//	POST /_fake/refunds/{id}/settle?success=true|false
//	POST /_fake/events/{id}/redeliver
//
// Paying delivers a signed webhook to the endpoints the account registered
// (POST /api/webhooks); *.localhost endpoints are reached on 127.0.0.1.
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

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/yoco/yocotest"
)

func main() {
	addr := flag.String("addr", ":12112", "listen address")
	base := flag.String("base", "", "address customers reach the fake on, for redirectUrl (default: the request's host)")
	flag.Parse()

	fake := yocotest.New()
	fake.Base = *base
	srv := &http.Server{Addr: *addr, Handler: fake, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown) //nolint:contextcheck // the signal context is already done
	}()

	slog.Info("fake Yoco listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "fake-yoco:", err)
		os.Exit(1)
	}
}
