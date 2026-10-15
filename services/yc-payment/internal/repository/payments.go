// Package repository is yc-payment's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
)

// Payments reads and writes the payment tables.
type Payments struct{}

// StoredGatewayConfig is a gateway config row with its sealed credentials.
type StoredGatewayConfig struct {
	domain.GatewayConfig
	CredentialsEnc []byte
}

const gatewayColumns = `gateway, enabled, credentials_enc, api_key_hint, webhook_secret_set, COALESCE(public_key, ''),
	created_at, updated_at`

func scanGateway(r pgx.Row) (StoredGatewayConfig, error) {
	var c StoredGatewayConfig
	err := r.Scan(&c.Gateway, &c.Enabled, &c.CredentialsEnc, &c.APIKeyHint, &c.WebhookSecretSet, &c.PublicKey,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// GatewayConfigs returns the shop's gateway settings.
func (Payments) GatewayConfigs(ctx context.Context, tx pgx.Tx, tenantID string) ([]StoredGatewayConfig, error) {
	rows, err := tx.Query(ctx, `SELECT `+gatewayColumns+` FROM tenant_gateway_config WHERE tenant_id = $1 ORDER BY gateway`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("list gateway configs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StoredGatewayConfig, error) { return scanGateway(r) })
	if err != nil {
		return nil, fmt.Errorf("list gateway configs: %w", err)
	}
	return out, nil
}

// GatewayConfig returns the shop's settings for gw, or false.
func (Payments) GatewayConfig(ctx context.Context, tx pgx.Tx, tenantID, gw string) (StoredGatewayConfig, bool, error) {
	c, err := scanGateway(tx.QueryRow(ctx, `SELECT `+gatewayColumns+` FROM tenant_gateway_config
		WHERE tenant_id = $1 AND gateway = $2`, tenantID, gw))
	if dbkit.IsNoRows(err) {
		return StoredGatewayConfig{}, false, nil
	}
	if err != nil {
		return StoredGatewayConfig{}, false, fmt.Errorf("get gateway config: %w", err)
	}
	return c, true, nil
}

// PutGatewayConfig creates or replaces the shop's settings for a gateway.
func (Payments) PutGatewayConfig(ctx context.Context, tx pgx.Tx, tenantID string, c StoredGatewayConfig) error {
	_, err := tx.Exec(ctx, `INSERT INTO tenant_gateway_config (tenant_id, gateway, enabled, credentials_enc, api_key_hint,
			webhook_secret_set, public_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $8)
		ON CONFLICT (tenant_id, gateway) DO UPDATE SET enabled = EXCLUDED.enabled,
			credentials_enc = EXCLUDED.credentials_enc, api_key_hint = EXCLUDED.api_key_hint,
			webhook_secret_set = EXCLUDED.webhook_secret_set, public_key = EXCLUDED.public_key,
			updated_at = EXCLUDED.updated_at`,
		tenantID, c.Gateway, c.Enabled, c.CredentialsEnc, c.APIKeyHint, c.WebhookSecretSet, c.PublicKey, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put gateway config: %w", err)
	}
	return nil
}

const transactionColumns = `id, session_id, COALESCE(order_id::text, ''), gateway, COALESCE(gateway_reference, ''), status,
	amount_minor, currency, captured_amount_minor, refunded_amount_minor, idempotency_key, next_action,
	COALESCE(failure_reason, ''), created_at, updated_at`

func scanTransaction(r pgx.Row) (domain.Transaction, error) {
	var t domain.Transaction
	var status string
	var next []byte
	err := r.Scan(&t.ID, &t.SessionID, &t.OrderID, &t.Gateway, &t.GatewayReference, &status,
		&t.AmountMinor, &t.Currency, &t.CapturedMinor, &t.RefundedMinor, &t.IdempotencyKey, &next,
		&t.FailureReason, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.Status = domain.Status(status)
	if len(next) > 0 {
		if err := json.Unmarshal(next, &t.NextAction); err != nil {
			return t, fmt.Errorf("decode next_action: %w", err)
		}
	}
	return t, nil
}

func (Payments) oneTransaction(ctx context.Context, tx pgx.Tx, where string, args ...any) (domain.Transaction, bool, error) {
	t, err := scanTransaction(tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM transactions WHERE `+where, args...))
	if dbkit.IsNoRows(err) {
		return domain.Transaction{}, false, nil
	}
	if err != nil {
		return domain.Transaction{}, false, fmt.Errorf("get transaction: %w", err)
	}
	return t, true, nil
}

// Transaction returns a transaction by ID; forUpdate locks it.
func (p Payments) Transaction(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Transaction, bool, error) {
	if !validUUID(id) {
		return domain.Transaction{}, false, nil
	}
	return p.oneTransaction(ctx, tx, `tenant_id = $1 AND id = $2`+lock(forUpdate), tenantID, id)
}

// TransactionByKey returns the transaction an authorization key made.
func (p Payments) TransactionByKey(ctx context.Context, tx pgx.Tx, tenantID, key string, forUpdate bool) (domain.Transaction, bool, error) {
	return p.oneTransaction(ctx, tx, `tenant_id = $1 AND idempotency_key = $2`+lock(forUpdate), tenantID, key)
}

// TransactionByReference returns the transaction for a gateway payment.
func (p Payments) TransactionByReference(ctx context.Context, tx pgx.Tx, tenantID, gw, ref string) (domain.Transaction, bool, error) {
	return p.oneTransaction(ctx, tx, `tenant_id = $1 AND gateway = $2 AND gateway_reference = $3 FOR UPDATE`,
		tenantID, gw, ref)
}

func lock(forUpdate bool) string {
	if forUpdate {
		return ` FOR UPDATE`
	}
	return ""
}

// InsertTransaction records a new transaction.
func (Payments) InsertTransaction(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Transaction) error {
	_, err := tx.Exec(ctx, `INSERT INTO transactions (id, tenant_id, session_id, order_id, gateway, status, amount_minor,
			currency, idempotency_key, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10, $10)`,
		t.ID, tenantID, t.SessionID, t.OrderID, t.Gateway, string(t.Status), t.AmountMinor, t.Currency,
		t.IdempotencyKey, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}
	return nil
}

// UpdateTransaction saves a transaction's mutable fields.
func (Payments) UpdateTransaction(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Transaction) error {
	var next []byte
	if t.NextAction != nil {
		var err error
		if next, err = json.Marshal(t.NextAction); err != nil {
			return fmt.Errorf("encode next_action: %w", err)
		}
	}
	_, err := tx.Exec(ctx, `UPDATE transactions SET order_id = NULLIF($3, '')::uuid, gateway_reference = NULLIF($4, ''),
			status = $5, captured_amount_minor = $6, refunded_amount_minor = $7, next_action = $8,
			failure_reason = NULLIF($9, ''), updated_at = $10
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, t.ID, t.OrderID, t.GatewayReference, string(t.Status), t.CapturedMinor, t.RefundedMinor, next,
		t.FailureReason, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update transaction: %w", err)
	}
	return nil
}

// TransactionFilter narrows ListTransactions.
type TransactionFilter struct {
	Status    string
	SessionID string
	OrderID   string
}

// TransactionCursor is the sort key of the last transaction on a page;
// lists run newest first.
type TransactionCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// ListTransactions returns up to limit transactions, newest first.
func (Payments) ListTransactions(ctx context.Context, tx pgx.Tx, tenantID string, f TransactionFilter, after *TransactionCursor, limit int) ([]domain.Transaction, error) {
	args := []any{tenantID, f.Status, f.SessionID, f.OrderID, limit}
	q := `SELECT ` + transactionColumns + ` FROM transactions WHERE tenant_id = $1
		AND ($2 = '' OR status = $2) AND ($3 = '' OR session_id = $3) AND ($4 = '' OR order_id::text = $4)`
	if after != nil {
		q += ` AND (created_at, id) < ($6, $7)`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $5`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Transaction, error) { return scanTransaction(r) })
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	return out, nil
}

const refundColumns = `id, transaction_id, COALESCE(order_id::text, ''), amount_minor, COALESCE(reason, ''),
	COALESCE(gateway_reference, ''), status, COALESCE(failure_reason, ''), COALESCE(actor_id, ''), idempotency_key,
	created_at, completed_at`

func scanRefund(r pgx.Row) (domain.Refund, error) {
	var x domain.Refund
	var status string
	err := r.Scan(&x.ID, &x.TransactionID, &x.OrderID, &x.AmountMinor, &x.Reason, &x.GatewayReference, &status,
		&x.FailureReason, &x.ActorID, &x.IdempotencyKey, &x.CreatedAt, &x.CompletedAt)
	x.Status = domain.RefundStatus(status)
	return x, err
}

func (Payments) oneRefund(ctx context.Context, tx pgx.Tx, where string, args ...any) (domain.Refund, bool, error) {
	x, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds WHERE `+where+` FOR UPDATE`, args...))
	if dbkit.IsNoRows(err) {
		return domain.Refund{}, false, nil
	}
	if err != nil {
		return domain.Refund{}, false, fmt.Errorf("get refund: %w", err)
	}
	return x, true, nil
}

// RefundByKey returns (and locks) the refund a key made.
func (p Payments) RefundByKey(ctx context.Context, tx pgx.Tx, tenantID, key string) (domain.Refund, bool, error) {
	return p.oneRefund(ctx, tx, `tenant_id = $1 AND idempotency_key = $2`, tenantID, key)
}

// RefundByReference returns (and locks) the refund for a gateway refund.
func (p Payments) RefundByReference(ctx context.Context, tx pgx.Tx, tenantID, ref string) (domain.Refund, bool, error) {
	return p.oneRefund(ctx, tx, `tenant_id = $1 AND gateway_reference = $2`, tenantID, ref)
}

// Refunds returns a transaction's refunds, oldest first.
func (Payments) Refunds(ctx context.Context, tx pgx.Tx, tenantID, transactionID string) ([]domain.Refund, error) {
	rows, err := tx.Query(ctx, `SELECT `+refundColumns+` FROM refunds WHERE tenant_id = $1 AND transaction_id = $2
		ORDER BY created_at, id`, tenantID, transactionID)
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Refund, error) { return scanRefund(r) })
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	return out, nil
}

// PendingRefundTotal sums a transaction's refunds still in flight.
func (Payments) PendingRefundTotal(ctx context.Context, tx pgx.Tx, tenantID, transactionID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM refunds
		WHERE tenant_id = $1 AND transaction_id = $2 AND status = 'pending'`, tenantID, transactionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sum pending refunds: %w", err)
	}
	return n, nil
}

// InsertRefund records a new refund.
func (Payments) InsertRefund(ctx context.Context, tx pgx.Tx, tenantID string, x domain.Refund) error {
	_, err := tx.Exec(ctx, `INSERT INTO refunds (id, transaction_id, tenant_id, order_id, amount_minor, reason, status,
			actor_id, idempotency_key, created_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, NULLIF($6, ''), $7, NULLIF($8, ''), $9, $10)`,
		x.ID, x.TransactionID, tenantID, x.OrderID, x.AmountMinor, x.Reason, string(x.Status), x.ActorID,
		x.IdempotencyKey, x.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert refund: %w", err)
	}
	return nil
}

// UpdateRefund saves a refund's outcome.
func (Payments) UpdateRefund(ctx context.Context, tx pgx.Tx, tenantID string, x domain.Refund) error {
	_, err := tx.Exec(ctx, `UPDATE refunds SET gateway_reference = NULLIF($3, ''), status = $4,
			failure_reason = NULLIF($5, ''), completed_at = $6
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, x.ID, x.GatewayReference, string(x.Status), x.FailureReason, x.CompletedAt)
	if err != nil {
		return fmt.Errorf("update refund: %w", err)
	}
	return nil
}

// RecordWebhook stores a webhook; false means it was seen before.
func (Payments) RecordWebhook(ctx context.Context, tx pgx.Tx, tenantID, gw, eventID, eventType string, payload []byte, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO webhook_events (gateway, gateway_event_id, tenant_id, event_type, received_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, gw, eventID, tenantID, eventType, now, payload)
	if err != nil {
		return false, fmt.Errorf("record webhook: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func validUUID(s string) bool { return uuid.Validate(s) == nil }
