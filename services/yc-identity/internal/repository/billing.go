package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Billing persists customer billing addresses (issue #29).
type Billing struct{}

// Get returns the billing address for userID, or NotFound.
func (Billing) Get(ctx context.Context, tx pgx.Tx, userID string) (domain.BillingAddress, error) {
	var b domain.BillingAddress
	err := tx.QueryRow(ctx, `
		SELECT company_name, vat_number, street_and_number, suburb, town_city, postal_code, country_code
		  FROM user_billing WHERE user_id = $1`, userID).Scan(
		&b.CompanyName, &b.VATNumber, &b.StreetAndNumber, &b.Suburb, &b.TownCity, &b.PostalCode, &b.CountryCode)
	if dbkit.IsNoRows(err) {
		return domain.BillingAddress{}, ycerrors.NotFound("Billing", userID)
	}
	if err != nil {
		return domain.BillingAddress{}, fmt.Errorf("get billing: %w", err)
	}
	return b, nil
}

// Put replaces the billing address for userID in tenantID.
func (Billing) Put(ctx context.Context, tx pgx.Tx, tenantID, userID string, b domain.BillingAddress, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO user_billing (user_id, tenant_id, company_name, vat_number, street_and_number, suburb,
			town_city, postal_code, country_code, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (user_id) DO UPDATE SET
			company_name = EXCLUDED.company_name, vat_number = EXCLUDED.vat_number,
			street_and_number = EXCLUDED.street_and_number, suburb = EXCLUDED.suburb,
			town_city = EXCLUDED.town_city, postal_code = EXCLUDED.postal_code,
			country_code = EXCLUDED.country_code, updated_at = EXCLUDED.updated_at`,
		userID, tenantID, b.CompanyName, b.VATNumber, b.StreetAndNumber, b.Suburb,
		b.TownCity, b.PostalCode, b.CountryCode, at)
	if err != nil {
		return fmt.Errorf("put billing: %w", err)
	}
	return nil
}

// Delete removes the billing address for userID.
func (Billing) Delete(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `DELETE FROM user_billing WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete billing: %w", err)
	}
	return nil
}
