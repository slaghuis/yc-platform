package service

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/repository"
)

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// maxKey bounds Idempotency-Key values (api-conventions.md §10).
const maxKey = 255

// PaymentRequest starts a checkout session's payment: an authorization
// (Variant A) or a combined charge (Variant B).
type PaymentRequest struct {
	SessionID   string
	Gateway     string
	AmountMinor int64
	Currency    string
	// PaymentToken is required unless the gateway hosts the payment page.
	PaymentToken string
	ReturnURL    string
	// OrderID is the pending order a charge pays for (Variant B).
	OrderID        string
	IdempotencyKey string
}

func (r PaymentRequest) validate(d gateway.Driver, kind domain.Kind) error {
	v := ycerrors.Validation()
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > maxKey {
		v.Field("Idempotency-Key", "required", "Idempotency-Key header is required (at most 255 characters)")
	}
	if r.SessionID == "" || len(r.SessionID) > 100 {
		v.Field("session_id", "required", "Session ID is required")
	}
	if r.AmountMinor <= 0 {
		v.Field("amount.amount_minor", "invalid", "Amount must be positive")
	}
	switch cur := d.SupportedCurrencies(); {
	case !currencyRE.MatchString(r.Currency):
		v.Field("amount.currency", "invalid", "Currency must be an ISO 4217 code")
	case cur != nil && !slices.Contains(cur, r.Currency):
		v.Field("amount.currency", "unsupported", d.Name()+" does not take "+r.Currency)
	}
	switch {
	case d.HostedPayment() && r.PaymentToken != "":
		v.Field("payment_token", "unexpected", d.Name()+" collects the payment method on its own page")
	case !d.HostedPayment() && (strings.TrimSpace(r.PaymentToken) == "" || len(r.PaymentToken) > 255):
		v.Field("payment_token", "required", "Payment token is required")
	}
	if len(r.ReturnURL) > 2048 {
		v.Field("return_url", "too_long", "Return URL is at most 2048 characters")
	}
	if d.HostedPayment() && r.ReturnURL == "" {
		v.Field("return_url", "required", "A return URL is required: the customer pays on "+d.Name()+"'s page")
	}
	if r.OrderID != "" && uuid.Validate(r.OrderID) != nil {
		v.Field("order_id", "invalid", "Order ID must be a UUID")
	}
	if kind == domain.KindAuthorize && !d.SupportsSeparateAuthCapture() {
		v.Field("gateway", "unsupported", d.Name()+" cannot authorize without capturing; charge instead (Variant B)")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Authorize holds the amount at the gateway (Variant A).
func (s *Service) Authorize(ctx context.Context, req PaymentRequest) (domain.Transaction, error) {
	return s.pay(ctx, domain.KindAuthorize, req)
}

// Charge takes the amount in one step (Variant B). A hosted payment (Yoco)
// is pending with a redirect next_action until the gateway's webhook
// reports the outcome.
func (s *Service) Charge(ctx context.Context, req PaymentRequest) (domain.Transaction, error) {
	return s.pay(ctx, domain.KindCharge, req)
}

// pay starts an authorization or charge. A repeated key returns the
// transaction's current state; one whose outcome is not known yet
// (pending 3-D Secure, or a call that failed) asks the gateway again,
// unless the gateway cannot be asked and reports by webhook only.
// Declines are a failed transaction, not an error; gateway outages are
// gateway_unavailable so the caller retries with the same key.
func (s *Service) pay(ctx context.Context, kind domain.Kind, req PaymentRequest) (domain.Transaction, error) {
	d, err := s.driver(req.Gateway)
	if err != nil {
		return domain.Transaction{}, err
	}
	if err := req.validate(d, kind); err != nil {
		return domain.Transaction{}, err
	}
	var t domain.Transaction
	var creds gateway.Credentials
	var tenantID string
	err = s.inShop(ctx, func(tx pgx.Tx, tenant string) error {
		tenantID = tenant
		existing, ok, err := s.repo.TransactionByKey(ctx, tx, tenant, req.IdempotencyKey, false)
		if err != nil {
			return err
		}
		if ok {
			if existing.SessionID != req.SessionID || existing.Gateway != req.Gateway || existing.Kind != kind ||
				existing.AmountMinor != req.AmountMinor || existing.Currency != req.Currency {
				return ycerrors.Conflict("Idempotency-Key was used for a different payment")
			}
			t = existing
			if t.Status.Settled() {
				return nil
			}
		}
		// A new payment needs the gateway switched on; one under way is
		// finished even if the shop has since switched it off.
		if creds, err = s.credentials(ctx, tx, tenant, req.Gateway, !ok); err != nil || ok {
			return err
		}
		now := s.now().UTC()
		t = domain.Transaction{
			ID: uuid.NewString(), SessionID: req.SessionID, OrderID: req.OrderID, Kind: kind, Gateway: req.Gateway,
			Status: domain.StatusCreated, AmountMinor: req.AmountMinor, Currency: req.Currency,
			IdempotencyKey: req.IdempotencyKey, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.InsertTransaction(ctx, tx, tenant, t); err != nil {
			if dbkit.IsUniqueViolation(err) {
				return ycerrors.Conflict("A payment with this Idempotency-Key is in progress")
			}
			return err
		}
		return s.publish(ctx, tx, tenant, events.TypePaymentIntentCreatedV1, events.PaymentIntentCreatedV1{
			TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: money(t.AmountMinor, t.Currency),
			CreatedAt: now,
		})
	})
	if err != nil || t.Status.Settled() {
		return t, err
	}

	var p gateway.Payment
	meta := map[string]string{"tenant_id": tenantID, "session_id": t.SessionID, "transaction_id": t.ID}
	switch {
	case t.GatewayReference != "":
		p, err = d.Get(ctx, creds, t.GatewayReference)
		if errors.Is(err, gateway.ErrUnsupported) {
			return t, nil
		}
	case kind == domain.KindCharge:
		p, err = d.Charge(ctx, creds, gateway.ChargeRequest{
			AmountMinor: t.AmountMinor, Currency: t.Currency, PaymentToken: req.PaymentToken, ReturnURL: req.ReturnURL,
			Metadata: meta, IdempotencyKey: "yc-" + t.ID + "-charge",
		})
	default:
		p, err = d.Authorize(ctx, creds, gateway.AuthorizeRequest{
			AmountMinor: t.AmountMinor, Currency: t.Currency, PaymentToken: req.PaymentToken, ReturnURL: req.ReturnURL,
			Metadata: meta, IdempotencyKey: "yc-" + t.ID + "-authorize",
		})
	}
	if p, err = declined(p, err); err != nil {
		return domain.Transaction{}, err
	}
	return s.record(ctx, tenantID, t.ID, p, "")
}

// declined turns a gateway refusal into a failed payment.
func declined(p gateway.Payment, err error) (gateway.Payment, error) {
	var de *gateway.DeclineError
	if !errors.As(err, &de) {
		return p, err
	}
	if de.Payment != nil {
		return *de.Payment, nil
	}
	return gateway.Payment{Status: gateway.StatusFailed, FailureReason: de.Code}, nil
}

// record applies the gateway's view of a payment and publishes the
// transition, if any.
func (s *Service) record(ctx context.Context, tenantID, id string, p gateway.Payment, orderID string) (domain.Transaction, error) {
	var t domain.Transaction
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var ok bool
		var err error
		t, ok, err = s.repo.Transaction(ctx, tx, tenantID, id, true)
		if err != nil || !ok {
			return errors.Join(err, ycerrors.NotFound("transaction", id))
		}
		return s.apply(ctx, tx, tenantID, &t, p, orderID)
	})
	return t, err
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, tenantID string, t *domain.Transaction, p gateway.Payment, orderID string) error {
	before := t.Status
	now := s.now().UTC()
	changed := t.Apply(p, now)
	if orderID != "" && t.OrderID != orderID {
		t.OrderID = orderID
		t.UpdatedAt = now
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.repo.UpdateTransaction(ctx, tx, tenantID, *t); err != nil {
		return err
	}
	if before == t.Status {
		return nil
	}
	return s.publishTransition(ctx, tx, tenantID, before, *t, now)
}

func (s *Service) publishTransition(ctx context.Context, tx pgx.Tx, tenantID string, before domain.Status, t domain.Transaction, now time.Time) error {
	amount := money(t.AmountMinor, t.Currency)
	if t.Kind == domain.KindCharge {
		switch t.Status {
		case domain.StatusCaptured:
			return s.publish(ctx, tx, tenantID, events.TypePaymentChargedV1, events.PaymentChargedV1{
				TransactionID: t.ID, SessionID: t.SessionID, OrderID: optional(t.OrderID), Gateway: t.Gateway,
				GatewayReference: optional(t.GatewayReference), Amount: money(t.CapturedMinor, t.Currency), ChargedAt: now,
			})
		case domain.StatusFailed:
			return s.publish(ctx, tx, tenantID, events.TypePaymentChargeFailedV1, events.PaymentChargeFailedV1{
				TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: amount,
				Reason: reasonOr(t.FailureReason), FailedAt: now,
			})
		case domain.StatusCreated, domain.StatusPending, domain.StatusAuthorized, domain.StatusVoided, domain.StatusRefunded:
		}
	}
	switch t.Status {
	case domain.StatusAuthorized:
		return s.publish(ctx, tx, tenantID, events.TypePaymentAuthorizedV1, events.PaymentAuthorizedV1{
			TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, GatewayReference: t.GatewayReference,
			Amount: amount, AuthorizedAt: now,
		})
	case domain.StatusFailed:
		if before == domain.StatusAuthorized {
			return s.publish(ctx, tx, tenantID, events.TypePaymentCaptureFailedV1, events.PaymentCaptureFailedV1{
				TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: amount,
				Reason: reasonOr(t.FailureReason), FailedAt: now,
			})
		}
		return s.publish(ctx, tx, tenantID, events.TypePaymentAuthorizationFailedV1, events.PaymentAuthorizationFailedV1{
			TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: amount,
			Reason: reasonOr(t.FailureReason), FailedAt: now,
		})
	case domain.StatusCaptured:
		return s.publish(ctx, tx, tenantID, events.TypePaymentCapturedV1, events.PaymentCapturedV1{
			TransactionID: t.ID, SessionID: t.SessionID, OrderID: optional(t.OrderID), Gateway: t.Gateway,
			Amount: money(t.CapturedMinor, t.Currency), CapturedAt: now,
		})
	case domain.StatusVoided:
		return s.publish(ctx, tx, tenantID, events.TypePaymentVoidedV1, events.PaymentVoidedV1{
			TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: amount, VoidedAt: now,
		})
	case domain.StatusCreated, domain.StatusPending, domain.StatusRefunded:
	}
	return nil
}

// CaptureRequest captures an authorized transaction.
type CaptureRequest struct {
	TransactionID string
	// AmountMinor captures less than authorized; 0 captures it all.
	AmountMinor int64
	OrderID     string
}

// Capture takes the authorized funds. Capturing a captured transaction
// returns it. A gateway refusal is payment_declined, with the transaction
// left as the gateway reports it.
func (s *Service) Capture(ctx context.Context, req CaptureRequest) (domain.Transaction, error) {
	if req.OrderID != "" && uuid.Validate(req.OrderID) != nil {
		return domain.Transaction{}, ycerrors.Validation().Field("order_id", "invalid", "Order ID must be a UUID").Build()
	}
	t, creds, tenantID, err := s.load(ctx, req.TransactionID)
	if err != nil {
		return domain.Transaction{}, err
	}
	switch t.Status {
	case domain.StatusCaptured, domain.StatusRefunded:
		return t, nil
	case domain.StatusAuthorized:
	case domain.StatusCreated, domain.StatusPending, domain.StatusVoided, domain.StatusFailed:
		return t, ycerrors.New(ycerrors.CodeInvalidStateTransition, "Only an authorized payment can be captured").
			With("transaction_status", string(t.Status))
	}
	if t.Kind == domain.KindCharge {
		return t, ycerrors.New(ycerrors.CodeInvalidStateTransition, "A charge is captured as it is made")
	}
	if req.AmountMinor < 0 || req.AmountMinor > t.AmountMinor {
		return t, ycerrors.Validation().Field("amount_minor", "invalid", "Capture amount exceeds the authorized amount").Build()
	}
	d, err := s.driver(t.Gateway)
	if err != nil {
		return t, err
	}
	p, err := d.Capture(ctx, creds, t.GatewayReference, req.AmountMinor, "yc-"+t.ID+"-capture")
	var de *gateway.DeclineError
	switch {
	case errors.As(err, &de):
		reason := de.Code
		if de.Payment != nil && de.Payment.Status != gateway.StatusAuthorized {
			if t, err = s.record(ctx, tenantID, t.ID, *de.Payment, req.OrderID); err != nil {
				return t, err
			}
			if t.Status == domain.StatusCaptured {
				return t, nil
			}
		} else if err := s.captureFailed(ctx, tenantID, t, reason); err != nil {
			return t, err
		}
		return t, ycerrors.New(ycerrors.CodePaymentDeclined, "The gateway refused the capture").
			With("transaction_id", t.ID).With("reason", reason)
	case err != nil:
		return t, err
	}
	return s.record(ctx, tenantID, t.ID, p, req.OrderID)
}

// captureFailed announces a refused capture that left the authorization
// in place.
func (s *Service) captureFailed(ctx context.Context, tenantID string, t domain.Transaction, reason string) error {
	return s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		return s.publish(ctx, tx, tenantID, events.TypePaymentCaptureFailedV1, events.PaymentCaptureFailedV1{
			TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Amount: money(t.AmountMinor, t.Currency),
			Reason: reason, FailedAt: s.now().UTC(),
		})
	})
}

// VoidRequest names the authorization to void, by transaction ID or by
// the Idempotency-Key that authorized it.
type VoidRequest struct {
	TransactionID    string
	AuthorizationKey string
}

// Void releases an authorization or cancels an unfinished charge. Nothing
// to void (no such payment, already voided or failed) is success: found is
// false when there was no transaction at all. A charge the gateway cannot
// cancel (a Yoco checkout) is returned still pending: the customer can
// still pay it, and the payment's owner refunds money that arrives late.
func (s *Service) Void(ctx context.Context, req VoidRequest) (t domain.Transaction, found bool, err error) {
	if req.TransactionID == "" && req.AuthorizationKey == "" {
		return t, false, ycerrors.Validation().Field("transaction_id", "required", "transaction_id or authorization_key is required").Build()
	}
	var creds gateway.Credentials
	var tenantID string
	err = s.inShop(ctx, func(tx pgx.Tx, tenant string) error {
		tenantID = tenant
		var err error
		if req.TransactionID != "" {
			t, found, err = s.repo.Transaction(ctx, tx, tenant, req.TransactionID, false)
		} else {
			t, found, err = s.repo.TransactionByKey(ctx, tx, tenant, req.AuthorizationKey, false)
		}
		if err != nil || !found {
			return err
		}
		if t.Status == domain.StatusPending || t.Status == domain.StatusAuthorized {
			creds, err = s.credentials(ctx, tx, tenant, t.Gateway, false)
		}
		return err
	})
	if err != nil || !found {
		return t, found, err
	}
	switch t.Status {
	case domain.StatusVoided, domain.StatusFailed:
		return t, true, nil
	case domain.StatusCaptured, domain.StatusRefunded:
		return t, true, ycerrors.New(ycerrors.CodeInvalidStateTransition, "A captured payment is refunded, not voided").
			With("transaction_status", string(t.Status))
	case domain.StatusCreated:
		// The gateway never answered, so there is no payment to cancel;
		// an orphaned hold lapses at the gateway.
		telemetry.LoggerFromContext(ctx).Warn("voiding a transaction the gateway never confirmed", "transaction_id", t.ID)
		t, err = s.record(ctx, tenantID, t.ID, gateway.Payment{Status: gateway.StatusVoided}, "")
		return t, true, err
	case domain.StatusPending, domain.StatusAuthorized:
	}
	d, err := s.driver(t.Gateway)
	if err != nil {
		return t, true, err
	}
	p, err := d.Void(ctx, creds, t.GatewayReference, "yc-"+t.ID+"-void")
	var de *gateway.DeclineError
	switch {
	case errors.Is(err, gateway.ErrUnsupported):
		if t.Status == domain.StatusAuthorized {
			return t, true, ycerrors.Internal(t.Gateway + " cannot void an authorization")
		}
		telemetry.LoggerFromContext(ctx).Info("gateway cannot cancel the payment; it stays open",
			"transaction_id", t.ID, "gateway", t.Gateway)
		return t, true, nil
	case errors.As(err, &de) && de.Payment != nil && de.Payment.Status != gateway.StatusAuthorized &&
		de.Payment.Status != gateway.StatusPending:
		// Already settled at the gateway (voided, captured or failed).
		t, err = s.record(ctx, tenantID, t.ID, *de.Payment, "")
		if err == nil && t.Status == domain.StatusCaptured {
			err = ycerrors.New(ycerrors.CodeInvalidStateTransition, "The payment was captured at the gateway").
				With("transaction_status", string(t.Status))
		}
		return t, true, err
	case errors.As(err, &de):
		if perr := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
			return s.publish(ctx, tx, tenantID, events.TypePaymentVoidFailedV1, events.PaymentVoidFailedV1{
				TransactionID: t.ID, SessionID: t.SessionID, Gateway: t.Gateway, Reason: de.Code, FailedAt: s.now().UTC(),
			})
		}); perr != nil {
			return t, true, perr
		}
		return t, true, ycerrors.New(ycerrors.CodeInvalidStateTransition, "The gateway refused the void").
			With("transaction_id", t.ID).With("reason", de.Code)
	case err != nil:
		return t, true, err
	}
	t, err = s.record(ctx, tenantID, t.ID, p, "")
	return t, true, err
}

// load reads a transaction and its shop's credentials.
func (s *Service) load(ctx context.Context, id string) (t domain.Transaction, creds gateway.Credentials, tenantID string, err error) {
	err = s.inShop(ctx, func(tx pgx.Tx, tenant string) error {
		tenantID = tenant
		var ok bool
		var err error
		t, ok, err = s.repo.Transaction(ctx, tx, tenant, id, false)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("transaction", id)
		}
		creds, err = s.credentials(ctx, tx, tenant, t.Gateway, false)
		return err
	})
	return t, creds, tenantID, err
}

// Transaction returns one of the caller's shop's transactions and its
// refunds.
func (s *Service) Transaction(ctx context.Context, id string) (domain.Transaction, []domain.Refund, error) {
	var t domain.Transaction
	var refunds []domain.Refund
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var ok bool
		var err error
		t, ok, err = s.repo.Transaction(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("transaction", id)
		}
		refunds, err = s.repo.Refunds(ctx, tx, tenantID, id)
		return err
	})
	return t, refunds, err
}

// ListTransactions returns a page of the shop admin's transactions,
// newest first.
func (s *Service) ListTransactions(ctx context.Context, f repository.TransactionFilter, after *repository.TransactionCursor, limit int) ([]domain.Transaction, error) {
	var out []domain.Transaction
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.ListTransactions(ctx, tx, tenantID, f, after, limit)
		return err
	})
	return out, err
}

func money(minor int64, currency string) events.Money {
	return events.Money{AmountMinor: minor, Currency: currency}
}

// reasonOr defaults an empty failure reason.
func reasonOr(reason string) string {
	if reason == "" {
		return "gateway_error"
	}
	return reason
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
