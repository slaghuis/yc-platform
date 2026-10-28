package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Pseudonymise gives userID's checkout sessions to pseudonym and clears
// their contact, address and return URL; amounts and lines stay
// (ADR-005). It returns how many sessions changed.
func (Sessions) Pseudonymise(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE checkout_sessions SET user_id = $3, guest_contact = NULL, delivery_address = NULL,
			billing_address = NULL, delivery_slot = NULL, return_url = NULL, payment_token = NULL
		WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID, pseudonym)
	if err != nil {
		return 0, fmt.Errorf("pseudonymise sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
