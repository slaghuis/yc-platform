// Package repository is yc-checkout's Postgres access. Every method runs
// in the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
)

// Sessions reads and writes the session tables.
type Sessions struct{}

const sessionColumns = `id, user_id, user_type, cart_type, COALESCE(cart_id::text, ''), state, items,
	requires_shipping, has_tracked_items, currency, tax_inclusive, items_total_minor, tax_total_minor,
	shipping_minor, total_minor, pricing_snapshot, shipping_options, COALESCE(shipping_option_id, ''),
	guest_contact, delivery_address, COALESCE(confirm_key, ''), COALESCE(payment_gateway, ''),
	COALESCE(payment_token, ''), COALESCE(return_url, ''), COALESCE(reservation_id::text, ''),
	reservation_expires_at, COALESCE(payment_transaction_id::text, ''), COALESCE(payment_status, ''),
	next_action, COALESCE(order_id::text, ''), COALESCE(order_number, ''), COALESCE(failed_state, ''),
	COALESCE(failure_reason, ''), failure_detail, COALESCE(end_state, ''), compensation_log, attempts,
	next_attempt_at, COALESCE(lease_owner, ''), lease_until, expires_at, confirmed_at, completed_at,
	created_at, updated_at, payment_variant, hosted_payment, COALESCE(payment_public_key, '')`

func scanSession(r pgx.Row) (domain.Session, error) {
	var s domain.Session
	var userType, state, failedState, endState, variant string
	var items, shipping, guest, address, nextAction, detail, compensation []byte
	err := r.Scan(&s.ID, &s.UserID, &userType, &s.CartType, &s.CartID, &state, &items,
		&s.RequiresShipping, &s.HasTrackedItems, &s.Currency, &s.TaxInclusive, &s.ItemsTotalMinor, &s.TaxTotalMinor,
		&s.ShippingMinor, &s.TotalMinor, &s.PricingSnapshot, &shipping, &s.ShippingOptionID,
		&guest, &address, &s.ConfirmKey, &s.PaymentGateway,
		&s.PaymentToken, &s.ReturnURL, &s.ReservationID,
		&s.ReservationExpiresAt, &s.PaymentTransactionID, &s.PaymentStatus,
		&nextAction, &s.OrderID, &s.OrderNumber, &failedState,
		&s.FailureReason, &detail, &endState, &compensation, &s.Attempts,
		&s.NextAttemptAt, &s.LeaseOwner, &s.LeaseUntil, &s.ExpiresAt, &s.ConfirmedAt, &s.CompletedAt,
		&s.CreatedAt, &s.UpdatedAt, &variant, &s.HostedPayment, &s.PaymentPublicKey)
	if err != nil {
		return s, err
	}
	s.UserType, s.State, s.PaymentVariant = domain.UserType(userType), domain.State(state), domain.Variant(variant)
	s.FailedState, s.EndState = domain.State(failedState), domain.State(endState)
	for name, f := range map[string]struct {
		raw []byte
		out any
	}{
		"items": {items, &s.Items}, "shipping_options": {shipping, &s.ShippingOptions},
		"guest_contact": {guest, &s.GuestContact}, "delivery_address": {address, &s.DeliveryAddress},
		"next_action": {nextAction, &s.NextAction}, "failure_detail": {detail, &s.FailureDetail},
		"compensation_log": {compensation, &s.CompensationLog},
	} {
		if len(f.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(f.raw, f.out); err != nil {
			return s, fmt.Errorf("decode %s: %w", name, err)
		}
	}
	return s, nil
}

// Session returns a session; forUpdate locks it.
func (Sessions) Session(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Session, bool, error) {
	if !validUUID(id) {
		return domain.Session{}, false, nil
	}
	q := `SELECT ` + sessionColumns + ` FROM checkout_sessions WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanSession(tx.QueryRow(ctx, q, tenantID, id))
	if dbkit.IsNoRows(err) {
		return domain.Session{}, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("get session: %w", err)
	}
	return s, true, nil
}

// encode marshals v, or returns NULL for an empty value.
func encode(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return nil, nil
		}
	case *domain.GuestContact:
		if x == nil {
			return nil, nil
		}
	}
	return json.Marshal(v)
}

type encoded struct {
	items, shipping, guest, address, nextAction, detail, compensation []byte
}

func encodeSession(s domain.Session) (encoded, error) {
	var e encoded
	var err error
	if s.ShippingOptions == nil {
		s.ShippingOptions = []domain.ShippingOption{}
	}
	if s.CompensationLog == nil {
		s.CompensationLog = []domain.CompensationEntry{}
	}
	for name, f := range map[string]struct {
		v   any
		out *[]byte
	}{
		"items": {s.Items, &e.items}, "shipping_options": {s.ShippingOptions, &e.shipping},
		"guest_contact": {s.GuestContact, &e.guest}, "delivery_address": {s.DeliveryAddress, &e.address},
		"next_action": {s.NextAction, &e.nextAction}, "failure_detail": {s.FailureDetail, &e.detail},
		"compensation_log": {s.CompensationLog, &e.compensation},
	} {
		if *f.out, err = encode(f.v); err != nil {
			return e, fmt.Errorf("encode %s: %w", name, err)
		}
	}
	return e, nil
}

// InsertSession records a new session.
func (Sessions) InsertSession(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Session) error {
	e, err := encodeSession(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO checkout_sessions (id, tenant_id, user_id, user_type, cart_type, cart_id,
			state, items, requires_shipping, has_tracked_items, currency, tax_inclusive, items_total_minor,
			tax_total_minor, shipping_minor, total_minor, pricing_snapshot, shipping_options, shipping_option_id,
			expires_at, created_at, updated_at, payment_gateway, payment_variant, hosted_payment, payment_public_key)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
			NULLIF($19, ''), $20, $21, $21, NULLIF($22, ''), $23, $24, NULLIF($25, ''))`,
		s.ID, tenantID, s.UserID, string(s.UserType), s.CartType, s.CartID,
		string(s.State), e.items, s.RequiresShipping, s.HasTrackedItems, s.Currency, s.TaxInclusive, s.ItemsTotalMinor,
		s.TaxTotalMinor, s.ShippingMinor, s.TotalMinor, []byte(s.PricingSnapshot), e.shipping, s.ShippingOptionID,
		s.ExpiresAt, s.CreatedAt, s.PaymentGateway, string(s.Variant()), s.HostedPayment, s.PaymentPublicKey)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// UpdateSession saves everything that changes after the session starts.
func (Sessions) UpdateSession(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Session) error {
	e, err := encodeSession(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE checkout_sessions SET state = $3, shipping_minor = $4, total_minor = $5,
			shipping_option_id = NULLIF($6, ''), guest_contact = $7, delivery_address = $8,
			confirm_key = NULLIF($9, ''), payment_gateway = NULLIF($10, ''), payment_token = NULLIF($11, ''),
			return_url = NULLIF($12, ''), reservation_id = NULLIF($13, '')::uuid, reservation_expires_at = $14,
			payment_transaction_id = NULLIF($15, '')::uuid, payment_status = NULLIF($16, ''), next_action = $17,
			order_id = NULLIF($18, '')::uuid, order_number = NULLIF($19, ''), failed_state = NULLIF($20, ''),
			failure_reason = NULLIF($21, ''), failure_detail = $22, end_state = NULLIF($23, ''),
			compensation_log = $24, attempts = $25, next_attempt_at = $26, lease_owner = NULLIF($27, ''),
			lease_until = $28, confirmed_at = $29, completed_at = $30, updated_at = $31, payment_variant = $32,
			hosted_payment = $33, payment_public_key = NULLIF($34, '')
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, s.ID, string(s.State), s.ShippingMinor, s.TotalMinor,
		s.ShippingOptionID, e.guest, e.address,
		s.ConfirmKey, s.PaymentGateway, s.PaymentToken,
		s.ReturnURL, s.ReservationID, s.ReservationExpiresAt,
		s.PaymentTransactionID, s.PaymentStatus, e.nextAction,
		s.OrderID, s.OrderNumber, string(s.FailedState),
		s.FailureReason, e.detail, string(s.EndState),
		e.compensation, s.Attempts, s.NextAttemptAt, s.LeaseOwner,
		s.LeaseUntil, s.ConfirmedAt, s.CompletedAt, s.UpdatedAt, string(s.Variant()),
		s.HostedPayment, s.PaymentPublicKey)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	return nil
}

// Event is one entry in a session's history.
type Event struct {
	ID        string
	EventType string
	From      domain.State
	To        domain.State
	Payload   map[string]any
	CreatedAt time.Time
}

// AddEvent appends to a session's history.
func (Sessions) AddEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID string, e Event) error {
	payload, err := encode(e.Payload)
	if err != nil {
		return fmt.Errorf("encode session event: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO session_events (id, tenant_id, session_id, event_type, from_state, to_state,
			payload, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8)`,
		uuid.NewString(), tenantID, sessionID, e.EventType, string(e.From), string(e.To), payload, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert session event: %w", err)
	}
	return nil
}

// Events returns a session's history, oldest first.
func (Sessions) Events(ctx context.Context, tx pgx.Tx, tenantID, sessionID string) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT id, event_type, COALESCE(from_state, ''), COALESCE(to_state, ''), payload,
			created_at
		FROM session_events WHERE tenant_id = $1 AND session_id = $2 ORDER BY created_at, id`, tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var from, to string
		var payload []byte
		if err := rows.Scan(&e.ID, &e.EventType, &from, &to, &payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan session event: %w", err)
		}
		e.From, e.To = domain.State(from), domain.State(to)
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &e.Payload); err != nil {
				return nil, fmt.Errorf("decode session event: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	return out, nil
}

// SessionCursor is the position after the last session of a page.
type SessionCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListSessions returns the shop's sessions, newest first, optionally in
// one state.
func (Sessions) ListSessions(ctx context.Context, tx pgx.Tx, tenantID string, state domain.State, after *SessionCursor, limit int) ([]domain.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM checkout_sessions WHERE tenant_id = $1`
	args := []any{tenantID}
	if state != "" {
		args = append(args, string(state))
		q += fmt.Sprintf(` AND state = $%d`, len(args))
	}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		q += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := make([]domain.Session, 0, limit)
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// Due is a session the sweeper should act on.
type Due struct {
	TenantID string
	ID       string
}

// ExpiredSessions returns up to limit unleased sessions past their TTL
// that have not reached the point of no return, across tenants (platform
// transaction).
func (Sessions) ExpiredSessions(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Due, error) {
	return due(ctx, tx, `state IN ('PRICING', 'RESERVING', 'AUTHORIZING', 'ORDER_CREATE', 'CHARGING', 'CAPTURING')
		AND expires_at <= $1 AND (lease_until IS NULL OR lease_until <= $1) ORDER BY expires_at LIMIT $2`, now, limit)
}

// ResumableSessions returns up to limit unleased sessions the saga still
// drives whose next attempt is due, across tenants (platform
// transaction).
func (Sessions) ResumableSessions(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Due, error) {
	return due(ctx, tx, `state IN ('RESERVING', 'AUTHORIZING', 'ORDER_CREATE', 'CHARGING', 'CAPTURING', 'CONFIRMING',
		'COMPENSATING')
		AND (next_attempt_at IS NULL OR next_attempt_at <= $1) AND (lease_until IS NULL OR lease_until <= $1)
		ORDER BY updated_at LIMIT $2`, now, limit)
}

func due(ctx context.Context, tx pgx.Tx, where string, now time.Time, limit int) ([]Due, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, id::text FROM checkout_sessions WHERE `+where, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due sessions: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Due])
	if err != nil {
		return nil, fmt.Errorf("list due sessions: %w", err)
	}
	return out, nil
}

// ReleaseLeases frees the leases a previous run of this replica held
// (owners starting with host but not with current), across tenants
// (platform transaction).
func (Sessions) ReleaseLeases(ctx context.Context, tx pgx.Tx, host, current string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE checkout_sessions SET lease_owner = NULL, lease_until = NULL
		WHERE starts_with(lease_owner, $1) AND NOT starts_with(lease_owner, $2)`, host, current)
	if err != nil {
		return 0, fmt.Errorf("release leases: %w", err)
	}
	return tag.RowsAffected(), nil
}

func validUUID(s string) bool { return uuid.Validate(s) == nil }
