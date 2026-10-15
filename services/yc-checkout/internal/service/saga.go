package service

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/integration"
)

// newToken is a lease token unique to one drive.
func (s *Service) newToken() string { return s.runID + uuid.NewString() }

// acquire takes the session's lease if no one holds it.
func (s *Service) acquire(ctx context.Context, tenantID, id string) (string, bool, error) {
	token := s.newToken()
	acquired := false
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		sess, ok, err := s.repo.Session(ctx, tx, tenantID, id, true)
		now := s.now().UTC()
		if err != nil || !ok || !sess.State.Driven() || sess.Leased(now) {
			return err
		}
		until := now.Add(s.cfg.LeaseTTL)
		sess.LeaseOwner, sess.LeaseUntil = token, &until
		acquired = true
		return s.repo.UpdateSession(ctx, tx, tenantID, sess)
	})
	return token, acquired, err
}

// release gives the lease up.
func (s *Service) release(ctx context.Context, tenantID, id, token string) {
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		sess, ok, err := s.repo.Session(ctx, tx, tenantID, id, true)
		if err != nil || !ok || sess.LeaseOwner != token {
			return err
		}
		sess.LeaseOwner, sess.LeaseUntil = "", nil
		return s.repo.UpdateSession(ctx, tx, tenantID, sess)
	})
	if err != nil {
		telemetry.LoggerFromContext(ctx).Warn("release checkout lease", "session_id", id, "err", err)
	}
}

// save changes a leased session in one transaction and renews the lease.
// A transition is recorded in the session's history. Saving outlives the
// caller's deadline: a call that returned is always recorded.
func (s *Service) save(ctx context.Context, tenantID, token, id string, fn func(tx pgx.Tx, sess *domain.Session) error) error {
	ctx = context.WithoutCancel(ctx)
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		sess, ok, err := s.repo.Session(ctx, tx, tenantID, id, true)
		if err != nil {
			return err
		}
		if !ok || sess.LeaseOwner != token {
			return errLeaseLost
		}
		from := sess.State
		if err := fn(tx, &sess); err != nil {
			return err
		}
		now := s.now().UTC()
		until := now.Add(s.cfg.LeaseTTL)
		sess.UpdatedAt, sess.LeaseUntil = now, &until
		if sess.State != from {
			if err := s.history(ctx, tx, tenantID, id, "state_changed", from, sess.State, nil); err != nil {
				return err
			}
		}
		return s.repo.UpdateSession(ctx, tx, tenantID, sess)
	})
}

// drive runs the saga until the session finishes, waits (on the
// customer, or for a background retry) or ctx ends. token is a lease the
// caller already holds; without one drive takes the lease, and returns
// the session as it is when someone else holds it.
func (s *Service) drive(ctx context.Context, tenantID, id, token string) (domain.Session, error) {
	if token == "" {
		var ok bool
		var err error
		if token, ok, err = s.acquire(ctx, tenantID, id); err != nil || !ok {
			if err != nil {
				return domain.Session{}, err
			}
			return s.load(ctx, tenantID, id)
		}
	}
	defer s.release(context.WithoutCancel(ctx), tenantID, id, token)
	for {
		sess, err := s.load(context.WithoutCancel(ctx), tenantID, id)
		if err != nil || !sess.State.Driven() || ctx.Err() != nil {
			return sess, err
		}
		if err := s.pause(ctx, sess); err != nil {
			return sess, nil
		}
		more, err := s.step(ctx, tenantID, token, sess)
		if err != nil || !more {
			out, lerr := s.load(context.WithoutCancel(ctx), tenantID, id)
			return out, errors.Join(err, lerr)
		}
	}
}

// pause stalls before the configured step (development only).
func (s *Service) pause(ctx context.Context, sess domain.Session) error {
	if s.cfg.PauseFor <= 0 || sess.State != s.cfg.PauseBefore {
		return nil
	}
	telemetry.LoggerFromContext(ctx).Warn("checkout paused before step (debug.pause_before)",
		"session_id", sess.ID, "state", sess.State, "for", s.cfg.PauseFor.String())
	return s.sleep(ctx, s.cfg.PauseFor)
}

// step runs the session's current step. more: the saga can go on.
func (s *Service) step(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	switch sess.State {
	case domain.StateReserving:
		return s.reserve(ctx, tenantID, token, sess)
	case domain.StateAuthorizing, domain.StateCharging:
		return s.pay(ctx, tenantID, token, sess)
	case domain.StateOrderCreate:
		return s.createOrder(ctx, tenantID, token, sess)
	case domain.StateCapturing:
		return s.capture(ctx, tenantID, token, sess)
	case domain.StateConfirming:
		return s.confirm(ctx, tenantID, token, sess)
	case domain.StateCompensating:
		return s.compensate(ctx, tenantID, token, sess)
	case domain.StatePricing, domain.StateCompleted, domain.StateFailed, domain.StateExpired:
	}
	return false, nil
}

// advance records the step's result and moves to the next state.
func (s *Service) advance(ctx context.Context, tenantID, token, id string, apply func(*domain.Session)) (bool, error) {
	err := s.save(ctx, tenantID, token, id, func(_ pgx.Tx, sess *domain.Session) error {
		apply(sess)
		sess.State = sess.Next()
		sess.Attempts, sess.NextAttemptAt = 0, nil
		return nil
	})
	return err == nil, err
}

// failed handles a step that did not succeed before the point of no
// return: a rejection or exhausted retries compensate (ADR-019 rule 6).
// A step cut short by the deadline is left for the sweeper.
func (s *Service) failed(ctx context.Context, tenantID, token string, sess domain.Session, err error, reason func(*integration.Rejection) (string, map[string]any)) (bool, error) {
	if interrupted(ctx) {
		return false, nil
	}
	why, detail := domain.ReasonStepFailed, describe(err)
	if r, ok := integration.Rejected(err); ok && reason != nil {
		why, detail = reason(r)
	}
	telemetry.LoggerFromContext(ctx).Info("checkout step failed; compensating",
		"session_id", sess.ID, "state", sess.State, "reason", why, "err", err)
	return s.startCompensation(ctx, tenantID, token, sess.ID, why, detail, domain.StateFailed)
}

func (s *Service) startCompensation(ctx context.Context, tenantID, token, id, reason string, detail map[string]any, end domain.State) (bool, error) {
	err := s.save(ctx, tenantID, token, id, func(tx pgx.Tx, sess *domain.Session) error {
		if !sess.State.Compensable() {
			return nil
		}
		return s.beginCompensation(ctx, tx, tenantID, sess, reason, detail, end)
	})
	return err == nil, err
}

// beginCompensation moves a session to COMPENSATING and announces it. The
// caller saves the session.
func (s *Service) beginCompensation(ctx context.Context, tx pgx.Tx, tenantID string, sess *domain.Session, reason string, detail map[string]any, end domain.State) error {
	now := s.now().UTC()
	if err := sess.Compensate(reason, detail, end, now); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeCheckoutCompensatingV1, events.CheckoutCompensatingV1{
		SessionID: sess.ID, UserID: sess.UserID, FailedState: string(sess.FailedState), Reason: reason,
		CompensatingAt: now,
	})
}

// retryLater schedules a background attempt for a step that must not be
// compensated (or a compensation that must finish).
func (s *Service) retryLater(ctx context.Context, tenantID, token string, sess domain.Session, cause error) (bool, error) {
	if interrupted(ctx) {
		return false, nil
	}
	err := s.save(ctx, tenantID, token, sess.ID, func(_ pgx.Tx, x *domain.Session) error {
		x.RetryLater(s.now().UTC(), s.cfg.RetryBase, s.cfg.RetryMax)
		sess.Attempts = x.Attempts
		return nil
	})
	log := telemetry.LoggerFromContext(ctx)
	if sess.Attempts >= s.cfg.AlertAfter && s.cfg.AlertAfter > 0 {
		log.Error("checkout session stuck; needs attention", "alert", "critical",
			"session_id", sess.ID, "state", sess.State, "attempts", sess.Attempts, "err", cause)
	} else {
		log.Warn("checkout step will be retried", "session_id", sess.ID, "state", sess.State, "err", cause)
	}
	return false, err
}

func (s *Service) reserve(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	res, err := s.p.Reserve(ctx, sess.Key("reserve"), sess.ID, lines(sess.TrackedItems()), minutes(s.cfg.ReservationTTL))
	if err != nil {
		return s.failed(ctx, tenantID, token, sess, err, func(r *integration.Rejection) (string, map[string]any) {
			if r.Problem.Code == "insufficient_stock" {
				return domain.ReasonInsufficientStock, map[string]any{"shortages": r.Problem.Extensions["shortages"]}
			}
			return domain.ReasonStepFailed, describe(r)
		})
	}
	return s.advance(ctx, tenantID, token, sess.ID, func(x *domain.Session) {
		x.ReservationID, x.ReservationExpiresAt = res.ID, res.ExpiresAt
	})
}

func lines(items []domain.Item) []integration.CartLine {
	out := make([]integration.CartLine, len(items))
	for i, it := range items {
		out[i] = integration.CartLine{SKUID: it.SKUID, Quantity: it.Quantity}
	}
	return out
}

func (s *Service) paymentRequest(sess domain.Session) integration.PaymentRequest {
	return integration.PaymentRequest{
		SessionID: sess.ID, Gateway: sess.PaymentGateway, PaymentToken: sess.PaymentToken, ReturnURL: sess.ReturnURL,
		Amount: money.Money{AmountMinor: sess.TotalMinor, Currency: sess.Currency}, OrderID: sess.OrderID,
	}
}

// pay authorises (Variant A) or charges (Variant B). A payment awaiting
// the customer (3-D Secure, a hosted payment page) waits for the
// webhook's event or the next poll; a decline compensates.
func (s *Service) pay(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	call, done := s.p.Authorize, []string{integration.PaymentAuthorized, integration.PaymentCaptured}
	if sess.Variant() == domain.VariantCharge {
		call, done = s.p.Charge, []string{integration.PaymentCaptured}
	}
	t, err := call(ctx, sess.Key(sess.PaymentCall()), s.paymentRequest(sess))
	if err != nil {
		return s.failed(ctx, tenantID, token, sess, err, nil)
	}
	record := func(x *domain.Session) {
		x.PaymentTransactionID, x.PaymentStatus, x.NextAction = t.ID, t.Status, t.NextAction
	}
	switch {
	case slices.Contains(done, t.Status):
		return s.advance(ctx, tenantID, token, sess.ID, func(x *domain.Session) {
			record(x)
			x.PaymentToken, x.NextAction = "", nil
		})
	case t.Status == integration.PaymentPending || t.Status == integration.PaymentCreated:
		err := s.save(ctx, tenantID, token, sess.ID, func(_ pgx.Tx, x *domain.Session) error {
			record(x)
			at := s.now().UTC().Add(s.cfg.PendingPoll)
			x.NextAttemptAt = &at
			return nil
		})
		return false, err
	}
	err = s.save(ctx, tenantID, token, sess.ID, func(tx pgx.Tx, x *domain.Session) error {
		record(x)
		x.NextAction = nil
		return s.beginCompensation(ctx, tx, tenantID, x, domain.ReasonPaymentDeclined,
			map[string]any{"decline_reason": t.FailureReason}, domain.StateFailed)
	})
	return err == nil, err
}

func (s *Service) orderInput(sess domain.Session) integration.CreateOrder {
	items := make([]integration.OrderItem, len(sess.Items))
	for i, it := range sess.Items {
		items[i] = integration.OrderItem{
			SKUID: it.SKUID, ProductID: it.ProductID, SKUCode: it.SKUCode, NameSnapshot: it.Name,
			VariantValuesSnapshot: it.VariantValues, Quantity: it.Quantity, UnitPriceMinor: it.UnitPriceMinor,
			LineTotalMinor: it.LineTotalMinor, TaxMinor: it.TaxMinor, IsDigital: it.IsDigital,
		}
	}
	in := integration.CreateOrder{
		CheckoutSessionID: sess.ID, UserID: sess.UserID, UserType: string(sess.UserType),
		RequiresShipping: sess.RequiresShipping, Items: items, Currency: sess.Currency,
		ItemsTotalMinor: sess.ItemsTotalMinor, ShippingMinor: sess.ShippingMinor, TaxTotalMinor: sess.TaxTotalMinor,
		TotalMinor: sess.TotalMinor, PricingSnapshot: sess.PricingSnapshot,
	}
	if g := sess.GuestContact; g != nil {
		in.GuestContact = &integration.GuestContact{Email: g.Email, PhoneE164: g.PhoneE164}
	}
	if sess.RequiresShipping {
		in.DeliveryAddress = sess.DeliveryAddress
		if o, ok := sess.SelectedShipping(); ok {
			in.ShippingOption = map[string]any{"id": o.ID, "method": o.ID, "name": o.Name, "amount_minor": o.AmountMinor}
			if o.RateID != "" {
				in.ShippingOption["rate_id"] = o.RateID
			}
		}
	}
	if sess.PaymentTransactionID != "" || sess.PaymentGateway != "" {
		in.Payment = &integration.OrderPayment{TransactionID: sess.PaymentTransactionID, Gateway: sess.PaymentGateway}
	}
	return in
}

func (s *Service) createOrder(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	o, err := s.p.CreateOrder(ctx, sess.Key("order"), s.orderInput(sess))
	if err != nil {
		return s.failed(ctx, tenantID, token, sess, err, nil)
	}
	return s.advance(ctx, tenantID, token, sess.ID, func(x *domain.Session) { x.OrderID = o.ID })
}

// capture is the point of no return. An unknown outcome is retried, never
// compensated blindly: the capture may have happened.
func (s *Service) capture(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	t, err := s.p.Capture(ctx, sess.Key("capture"), sess.PaymentTransactionID, sess.OrderID)
	if err != nil {
		if _, ok := integration.Rejected(err); !ok {
			return s.retryLater(ctx, tenantID, token, sess, err)
		}
		return s.failed(ctx, tenantID, token, sess, err, func(r *integration.Rejection) (string, map[string]any) {
			d := describe(r)
			if reason, ok := r.Problem.Extensions["reason"]; ok {
				d["decline_reason"] = reason
			}
			return domain.ReasonCaptureFailed, d
		})
	}
	switch t.Status {
	case integration.PaymentCaptured:
		return s.advance(ctx, tenantID, token, sess.ID, func(x *domain.Session) { x.PaymentStatus = t.Status })
	case integration.PaymentPending, integration.PaymentAuthorized, integration.PaymentCreated:
		return s.retryLater(ctx, tenantID, token, sess, errors.New("capture pending at the gateway"))
	}
	return s.startCompensation(ctx, tenantID, token, sess.ID, domain.ReasonCaptureFailed,
		map[string]any{"decline_reason": t.FailureReason, "payment_status": t.Status}, domain.StateFailed)
}

// confirm is forward-only: it is retried until yc-orders has placed the
// order and yc-inventory has committed the stock (ADR-019 rule 6).
func (s *Service) confirm(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	o, err := s.p.ConfirmOrder(ctx, sess.Key("confirm"), sess.OrderID, sess.PaymentTransactionID)
	if err != nil {
		return s.retryLater(ctx, tenantID, token, sess, err)
	}
	var skipped map[string]any
	if sess.ReservationID != "" {
		err := s.p.Commit(ctx, sess.Key("commit"), sess.ReservationID, sess.OrderID)
		if r, ok := integration.Rejected(err); ok && r.Status == http.StatusConflict {
			// Released before payment succeeded: the stock was already
			// returned, so there is nothing left to commit.
			skipped, err = describe(r), nil
		}
		if err != nil {
			return s.retryLater(ctx, tenantID, token, sess, err)
		}
	}
	err = s.save(ctx, tenantID, token, sess.ID, func(tx pgx.Tx, x *domain.Session) error {
		now := s.now().UTC()
		x.OrderNumber, x.State, x.CompletedAt = o.OrderNumber, domain.StateCompleted, &now
		x.Attempts, x.NextAttemptAt, x.PaymentToken = 0, nil, ""
		if skipped != nil {
			if err := s.history(ctx, tx, tenantID, x.ID, "commit_skipped", "", "", skipped); err != nil {
				return err
			}
		}
		return s.publish(ctx, tx, tenantID, events.TypeCheckoutCompletedV1, completed(*x, now))
	})
	return false, err
}

func completed(sess domain.Session, at time.Time) events.CheckoutCompletedV1 {
	items := make([]events.CheckoutCompletedV1ItemsItem, len(sess.Items))
	for i, it := range sess.Items {
		items[i] = events.CheckoutCompletedV1ItemsItem{SKUID: it.SKUID, Quantity: it.Quantity}
	}
	return events.CheckoutCompletedV1{
		SessionID: sess.ID, UserID: sess.UserID, CartType: sess.CartType, OrderID: sess.OrderID,
		OrderNumber: sess.OrderNumber, Items: items,
		Total: events.Money{AmountMinor: sess.TotalMinor, Currency: sess.Currency}, CompletedAt: at,
	}
}

// compensate undoes what the saga did, newest first: the payment, the
// order, the reservation. Each step is idempotent, treats "nothing to
// undo" as done and is recorded as it finishes. A payment that turns out
// to be captured sends the session forward to CONFIRMING instead. A
// hosted payment cannot be cancelled: it is left open, and if the
// customer pays after all, the late charge is refunded (LateCharge).
func (s *Service) compensate(ctx context.Context, tenantID, token string, sess domain.Session) (bool, error) {
	if sess.Reached(sess.PaymentState()) && !sess.Undone(domain.UndoPayment) {
		call := sess.PaymentCall()
		result, err := s.p.Void(ctx, sess.UndoKey(call), sess.PaymentTransactionID, sess.Key(call))
		if r, ok := integration.Rejected(err); ok && r.Status == http.StatusConflict {
			return s.capturedAfterAll(ctx, tenantID, token, sess, r)
		}
		if err != nil {
			return s.retryLater(ctx, tenantID, token, sess, err)
		}
		if result == integration.PaymentPending {
			result = "left_open"
		}
		return s.undone(ctx, tenantID, token, sess.ID, domain.UndoPayment, result)
	}
	if sess.Reached(domain.StateOrderCreate) && !sess.Undone(domain.UndoOrder) {
		result, err := s.abandonOrder(ctx, sess)
		if err != nil {
			return s.retryLater(ctx, tenantID, token, sess, err)
		}
		return s.undone(ctx, tenantID, token, sess.ID, domain.UndoOrder, result)
	}
	if sess.HasTrackedItems && sess.Reached(domain.StateReserving) && !sess.Undone(domain.UndoReservation) {
		result, err := s.releaseReservation(ctx, sess)
		if err != nil {
			return s.retryLater(ctx, tenantID, token, sess, err)
		}
		return s.undone(ctx, tenantID, token, sess.ID, domain.UndoReservation, result)
	}
	err := s.save(ctx, tenantID, token, sess.ID, func(tx pgx.Tx, x *domain.Session) error {
		from := x.FailedState
		x.Finish(s.now().UTC())
		return s.ended(ctx, tx, tenantID, *x, from)
	})
	return false, err
}

func (s *Service) undone(ctx context.Context, tenantID, token, id, step, result string) (bool, error) {
	err := s.save(ctx, tenantID, token, id, func(tx pgx.Tx, x *domain.Session) error {
		x.Undo(step, result, s.now().UTC())
		x.Attempts, x.NextAttemptAt = 0, nil
		return s.history(ctx, tx, tenantID, id, "compensated", "", "", map[string]any{"step": step, "result": result})
	})
	return err == nil, err
}

// capturedAfterAll: the void was refused because the payment was
// captured (a capture whose answer was lost). The session goes forward.
func (s *Service) capturedAfterAll(ctx context.Context, tenantID, token string, sess domain.Session, r *integration.Rejection) (bool, error) {
	id := sess.PaymentTransactionID
	if v, ok := r.Problem.Extensions["transaction_id"].(string); ok && id == "" {
		id = v
	}
	if id == "" {
		return s.retryLater(ctx, tenantID, token, sess, r)
	}
	t, err := s.p.Transaction(ctx, id)
	if err != nil || t.Status != integration.PaymentCaptured || sess.OrderID == "" {
		if err == nil {
			err = r
		}
		return s.retryLater(ctx, tenantID, token, sess, err)
	}
	telemetry.LoggerFromContext(ctx).Warn("payment was captured after all; confirming instead of compensating",
		"session_id", sess.ID)
	err = s.save(ctx, tenantID, token, sess.ID, func(tx pgx.Tx, x *domain.Session) error {
		x.PaymentTransactionID, x.PaymentStatus = t.ID, t.Status
		x.State, x.Attempts, x.NextAttemptAt = domain.StateConfirming, 0, nil
		return s.history(ctx, tx, tenantID, x.ID, "capture_discovered", "", "", map[string]any{"failure_reason": x.FailureReason})
	})
	return err == nil, err
}

// abandonOrder closes the pending order. When the create call's answer was
// lost, repeating it (same key) finds the order, if any.
func (s *Service) abandonOrder(ctx context.Context, sess domain.Session) (string, error) {
	id := sess.OrderID
	if id == "" {
		o, err := s.p.CreateOrder(ctx, sess.Key("order"), s.orderInput(sess))
		if _, ok := integration.Rejected(err); ok {
			return "none", nil
		}
		if err != nil {
			return "", err
		}
		id = o.ID
	}
	err := s.p.AbandonOrder(ctx, sess.UndoKey("order"), id, "checkout "+sess.FailureReason)
	if r, ok := integration.Rejected(err); ok {
		return "rejected: " + string(r.Problem.Code), nil
	}
	return "abandoned", err
}

// releaseReservation returns the held stock. When the reserve call's
// answer was lost, repeating it (same key) finds the reservation, if any.
func (s *Service) releaseReservation(ctx context.Context, sess domain.Session) (string, error) {
	id := sess.ReservationID
	if id == "" && sess.FailedState == domain.StateReserving && sess.FailureReason != domain.ReasonInsufficientStock {
		res, err := s.p.Reserve(ctx, sess.Key("reserve"), sess.ID, lines(sess.TrackedItems()), minutes(s.cfg.ReservationTTL))
		if _, ok := integration.Rejected(err); ok {
			return "none", nil
		}
		if err != nil {
			return "", err
		}
		id = res.ID
	}
	if id == "" {
		return "none", nil
	}
	err := s.p.Release(ctx, sess.UndoKey("reserve"), id)
	if r, ok := integration.Rejected(err); ok {
		return "rejected: " + string(r.Problem.Code), nil
	}
	return "released", err
}

// ended publishes the outcome of a session that finished without paying.
func (s *Service) ended(ctx context.Context, tx pgx.Tx, tenantID string, sess domain.Session, from domain.State) error {
	now := s.now().UTC()
	if sess.State == domain.StateExpired {
		return s.publish(ctx, tx, tenantID, events.TypeCheckoutExpiredV1, events.CheckoutExpiredV1{
			SessionID: sess.ID, UserID: sess.UserID, ExpiredState: string(from), ExpiredAt: now,
		})
	}
	return s.publish(ctx, tx, tenantID, events.TypeCheckoutFailedV1, events.CheckoutFailedV1{
		SessionID: sess.ID, UserID: sess.UserID, FailedState: string(from), Reason: sess.FailureReason, FailedAt: now,
	})
}

// interrupted: the run's deadline passed or its caller went away. The
// step's outcome is left for the sweeper.
func interrupted(ctx context.Context) bool { return ctx.Err() != nil }
