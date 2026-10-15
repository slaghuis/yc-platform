// Package repository is yc-message's Postgres access. Every function runs
// in the caller's transaction, under the tenant's row-level security.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// Messages is the repository. It is stateless.
type Messages struct{}

// Template returns a shop's template for a type, channel and locale, or
// false.
func (Messages) Template(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Type, ch domain.Channel, locale string) (domain.Template, bool, error) {
	row := tx.QueryRow(ctx, `SELECT `+templateColumns+` FROM templates
		WHERE tenant_id = $1 AND message_type = $2 AND channel = $3 AND locale = $4`,
		tenantID, string(t), string(ch), locale)
	tpl, err := scanTemplate(row)
	if dbkit.IsNoRows(err) {
		return tpl, false, nil
	}
	if err != nil {
		return tpl, false, fmt.Errorf("get template: %w", err)
	}
	return tpl, true, nil
}

// Templates returns a shop's own templates.
func (Messages) Templates(ctx context.Context, tx pgx.Tx, tenantID string) ([]domain.Template, error) {
	rows, err := tx.Query(ctx, `SELECT `+templateColumns+` FROM templates WHERE tenant_id = $1
		ORDER BY message_type, channel, locale`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Template, error) { return scanTemplate(r) })
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return out, nil
}

// PutTemplate creates or replaces a shop's template and returns it with
// its new version.
func (Messages) PutTemplate(ctx context.Context, tx pgx.Tx, t domain.Template) (domain.Template, error) {
	row := tx.QueryRow(ctx, `INSERT INTO templates (tenant_id, message_type, channel, locale, subject, body,
			body_format, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		ON CONFLICT (tenant_id, message_type, channel, locale) DO UPDATE SET subject = EXCLUDED.subject,
			body = EXCLUDED.body, body_format = EXCLUDED.body_format, version = templates.version + 1,
			updated_at = EXCLUDED.updated_at
		RETURNING `+templateColumns,
		t.TenantID, string(t.Type), string(t.Channel), t.Locale, t.Subject, t.Body, string(t.Format), t.UpdatedAt)
	out, err := scanTemplate(row)
	if err != nil {
		return out, fmt.Errorf("put template: %w", err)
	}
	return out, nil
}

// DeleteTemplate removes a shop's template, reporting whether it existed.
func (Messages) DeleteTemplate(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Type, ch domain.Channel, locale string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM templates
		WHERE tenant_id = $1 AND message_type = $2 AND channel = $3 AND locale = $4`,
		tenantID, string(t), string(ch), locale)
	if err != nil {
		return false, fmt.Errorf("delete template: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const templateColumns = `tenant_id, message_type, channel, locale, subject, body, body_format, version, updated_at`

func scanTemplate(r pgx.Row) (domain.Template, error) {
	var t domain.Template
	var typ, ch, format string
	err := r.Scan(&t.TenantID, &typ, &ch, &t.Locale, &t.Subject, &t.Body, &format, &t.Version, &t.UpdatedAt)
	t.Type, t.Channel, t.Format = domain.Type(typ), domain.Channel(ch), domain.Format(format)
	return t, err
}

// InsertMessage records a new message. It reports false, and changes
// nothing, when the shop already has a message with its reference.
func (Messages) InsertMessage(ctx context.Context, tx pgx.Tx, m domain.Message) (bool, error) {
	ctxJSON, err := json.Marshal(m.Context)
	if err != nil {
		return false, fmt.Errorf("encode message context: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO messages (id, tenant_id, message_type, recipient_user_id,
			recipient_address, channel, locale, subject, body_rendered, body_format, status, driver,
			next_attempt_at, context, reference, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)
		ON CONFLICT (tenant_id, reference) DO NOTHING`,
		m.ID, m.TenantID, string(m.Type), m.UserID, m.Address, string(m.Channel), m.Locale, m.Subject, m.Body,
		string(m.Format), string(m.Status), m.Driver, m.NextAttemptAt, ctxJSON, m.Reference, m.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert message: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateMessage saves a message's delivery state, body and context.
func (Messages) UpdateMessage(ctx context.Context, tx pgx.Tx, m domain.Message) error {
	ctxJSON, err := json.Marshal(m.Context)
	if err != nil {
		return fmt.Errorf("encode message context: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE messages SET status = $3, provider_message_id = NULLIF($4, ''), attempts = $5,
			last_error = NULLIF($6, ''), next_attempt_at = $7, body_rendered = $8, context = $9, updated_at = $10,
			sent_at = $11, delivered_at = $12
		WHERE tenant_id = $1 AND id = $2`,
		m.TenantID, m.ID, string(m.Status), m.ProviderMessageID, m.Attempts, m.LastError, m.NextAttemptAt,
		m.Body, ctxJSON, m.UpdatedAt, m.SentAt, m.DeliveredAt)
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	return nil
}

// Message returns a message by ID, locked when forUpdate, or false.
func (Messages) Message(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Message, bool, error) {
	return messageWhere(ctx, tx, `tenant_id = $1 AND id = $2`, forUpdate, tenantID, id)
}

// MessageByReference returns the message with a reference, or false.
func (Messages) MessageByReference(ctx context.Context, tx pgx.Tx, tenantID, reference string) (domain.Message, bool, error) {
	return messageWhere(ctx, tx, `tenant_id = $1 AND reference = $2`, false, tenantID, reference)
}

// MessageByProviderID returns the message a provider knows by id, locked,
// or false.
func (Messages) MessageByProviderID(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Message, bool, error) {
	return messageWhere(ctx, tx, `tenant_id = $1 AND provider_message_id = $2`, true, tenantID, id)
}

// LockDue returns a queued message that is due, locked, or false when it
// was sent meanwhile or another replica holds it.
func (Messages) LockDue(ctx context.Context, tx pgx.Tx, tenantID, id string, now time.Time) (domain.Message, bool, error) {
	q := `SELECT ` + messageColumns + ` FROM messages
		WHERE tenant_id = $1 AND id = $2 AND status = 'queued' AND next_attempt_at <= $3 FOR UPDATE SKIP LOCKED`
	m, err := scanMessage(tx.QueryRow(ctx, q, tenantID, id, now))
	if dbkit.IsNoRows(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("lock message: %w", err)
	}
	return m, true, nil
}

func messageWhere(ctx context.Context, tx pgx.Tx, where string, forUpdate bool, args ...any) (domain.Message, bool, error) {
	q := `SELECT ` + messageColumns + ` FROM messages WHERE ` + where
	if forUpdate {
		q += ` FOR UPDATE`
	}
	m, err := scanMessage(tx.QueryRow(ctx, q, args...))
	if dbkit.IsNoRows(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("get message: %w", err)
	}
	return m, true, nil
}

// Due is a queued message whose next attempt is due.
type Due struct {
	TenantID string
	ID       string
}

// DueMessages returns up to limit queued messages due at now, across
// shops, oldest first. It runs as the platform.
func (Messages) DueMessages(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Due, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, id FROM messages
		WHERE status = 'queued' AND next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due messages: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) {
		var d Due
		err := r.Scan(&d.TenantID, &d.ID)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("due messages: %w", err)
	}
	return out, nil
}

// MessageFilter narrows a message list.
type MessageFilter struct {
	Status domain.Status
	Type   domain.Type
	UserID string
}

// MessageCursor is the position after the last message of a page.
type MessageCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListMessages returns up to limit messages, newest first, after the
// cursor.
func (Messages) ListMessages(ctx context.Context, tx pgx.Tx, tenantID string, f MessageFilter, after *MessageCursor, limit int) ([]domain.Message, error) {
	q := `SELECT ` + messageColumns + ` FROM messages WHERE tenant_id = $1`
	args := []any{tenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Status != "" {
		add("status = $%d", string(f.Status))
	}
	if f.Type != "" {
		add("message_type = $%d", string(f.Type))
	}
	if f.UserID != "" {
		add("recipient_user_id = $%d", f.UserID)
	}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		q += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Message, error) { return scanMessage(r) })
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return out, nil
}

const messageColumns = `id, tenant_id, message_type, COALESCE(recipient_user_id, ''), recipient_address, channel,
	locale, subject, body_rendered, body_format, status, driver, COALESCE(provider_message_id, ''), attempts,
	COALESCE(last_error, ''), next_attempt_at, context, reference, created_at, updated_at, sent_at, delivered_at`

func scanMessage(r pgx.Row) (domain.Message, error) {
	var m domain.Message
	var typ, ch, format, status string
	var ctxJSON []byte
	err := r.Scan(&m.ID, &m.TenantID, &typ, &m.UserID, &m.Address, &ch, &m.Locale, &m.Subject, &m.Body, &format,
		&status, &m.Driver, &m.ProviderMessageID, &m.Attempts, &m.LastError, &m.NextAttemptAt, &ctxJSON,
		&m.Reference, &m.CreatedAt, &m.UpdatedAt, &m.SentAt, &m.DeliveredAt)
	if err != nil {
		return m, err
	}
	m.Type, m.Channel, m.Format, m.Status = domain.Type(typ), domain.Channel(ch), domain.Format(format), domain.Status(status)
	if err := json.Unmarshal(ctxJSON, &m.Context); err != nil {
		return m, fmt.Errorf("decode message context: %w", err)
	}
	return m, nil
}

// InsertVerification records a new verification code.
func (Messages) InsertVerification(ctx context.Context, tx pgx.Tx, v domain.Verification) error {
	_, err := tx.Exec(ctx, `INSERT INTO verifications (id, tenant_id, user_id, channel, address, code_hash,
			expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		v.ID, v.TenantID, v.UserID, string(v.Channel), v.Address, v.CodeHash, v.ExpiresAt, v.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert verification: %w", err)
	}
	return nil
}

// LatestVerification returns a user's newest verification for a channel,
// locked, or false.
func (Messages) LatestVerification(ctx context.Context, tx pgx.Tx, tenantID, userID string, ch domain.Channel) (domain.Verification, bool, error) {
	var v domain.Verification
	var channel string
	err := tx.QueryRow(ctx, `SELECT id, tenant_id, user_id, channel, address, code_hash, attempts, expires_at,
			confirmed_at, created_at
		FROM verifications WHERE tenant_id = $1 AND user_id = $2 AND channel = $3
		ORDER BY created_at DESC, id DESC LIMIT 1 FOR UPDATE`, tenantID, userID, string(ch)).
		Scan(&v.ID, &v.TenantID, &v.UserID, &channel, &v.Address, &v.CodeHash, &v.Attempts, &v.ExpiresAt,
			&v.ConfirmedAt, &v.CreatedAt)
	v.Channel = domain.Channel(channel)
	if dbkit.IsNoRows(err) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("get verification: %w", err)
	}
	return v, true, nil
}

// UpdateVerification saves a verification's attempts and confirmation.
func (Messages) UpdateVerification(ctx context.Context, tx pgx.Tx, v domain.Verification) error {
	_, err := tx.Exec(ctx, `UPDATE verifications SET attempts = $3, confirmed_at = $4 WHERE tenant_id = $1 AND id = $2`,
		v.TenantID, v.ID, v.Attempts, v.ConfirmedAt)
	if err != nil {
		return fmt.Errorf("update verification: %w", err)
	}
	return nil
}
