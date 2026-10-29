package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
)

// RedactedBody replaces message bodies erased under GDPR.
const RedactedBody = "[removed]"

// Messages reads and writes support_messages.
type Messages struct{}

// Insert stores a message on a request.
func (Messages) Insert(ctx context.Context, tx pgx.Tx, tenantID string, m domain.Message) error {
	_, err := tx.Exec(ctx, `INSERT INTO support_messages (tenant_id, id, request_id, author_type, author_user_id, body, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7)`,
		tenantID, m.ID, m.RequestID, string(m.AuthorType), m.AuthorUserID, m.Body, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert support message: %w", err)
	}
	return nil
}

// List returns a request's messages, oldest first.
func (Messages) List(ctx context.Context, tx pgx.Tx, tenantID, requestID string) ([]domain.Message, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, request_id::text, author_type, COALESCE(author_user_id::text, ''), body, created_at
		FROM support_messages WHERE tenant_id = $1 AND request_id = $2 ORDER BY created_at, id`, tenantID, requestID)
	if err != nil {
		return nil, fmt.Errorf("list support messages: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Message, error) {
		var m domain.Message
		err := r.Scan(&m.ID, &m.RequestID, &m.AuthorType, &m.AuthorUserID, &m.Body, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("list support messages: %w", err)
	}
	return out, nil
}

// Redact replaces the bodies, and detaches the authors, of the messages on
// requestIDs (GDPR). It returns how many messages it changed.
func (Messages) Redact(ctx context.Context, tx pgx.Tx, tenantID string, requestIDs []string) (int64, error) {
	if len(requestIDs) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE support_messages SET body = $3,
			author_user_id = CASE WHEN author_type IN ('customer', 'visitor') THEN NULL ELSE author_user_id END
		WHERE tenant_id = $1 AND request_id = ANY($2::uuid[])
			AND (body <> $3 OR (author_type IN ('customer', 'visitor') AND author_user_id IS NOT NULL))`,
		tenantID, requestIDs, RedactedBody)
	if err != nil {
		return 0, fmt.Errorf("redact support messages: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DetachAuthor clears userID as the author of messages in the shop, for a
// deleted shop admin (GDPR). Bodies stay: they belong to the thread.
func (Messages) DetachAuthor(ctx context.Context, tx pgx.Tx, tenantID, userID string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE support_messages SET author_user_id = NULL
		WHERE tenant_id = $1 AND author_user_id = $2`, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("detach support message author: %w", err)
	}
	return tag.RowsAffected(), nil
}
