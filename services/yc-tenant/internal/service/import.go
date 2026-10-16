package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

// ImportRecord is one shop reconstructed from yc-gateway hostnames and
// yc-pricing config. Import does not publish provisioned.v1 (ADR-029 rule 11).
type ImportRecord struct {
	TenantID            string
	Name                string
	Hostnames           []string // [0] is primary
	StoreCurrency       string
	DefaultJurisdiction string
	IsTaxInclusive      bool
	ContactEmail        string
}

// Import writes records that are not already present. Existing tenants are
// skipped. No lifecycle events are published.
func (s *Service) Import(ctx context.Context, records []ImportRecord) (created, skipped int, err error) {
	now := s.now().UTC()
	for _, r := range records {
		in := domain.CreateInput{
			TenantID: r.TenantID, Name: r.Name, Hostnames: r.Hostnames,
			StoreCurrency: r.StoreCurrency, DefaultJurisdiction: r.DefaultJurisdiction,
			IsTaxInclusive: r.IsTaxInclusive, ContactEmail: r.ContactEmail,
		}
		in, err = in.Validate()
		if err != nil {
			return created, skipped, fmt.Errorf("import %s: %w", r.TenantID, err)
		}
		if err := s.checkPlatformHosts(in.Hostnames); err != nil {
			return created, skipped, fmt.Errorf("import %s: %w", r.TenantID, err)
		}
		t := domain.Tenant{
			TenantID: in.TenantID, Name: in.Name, Status: domain.StatusActive,
			StoreCurrency: in.StoreCurrency, DefaultJurisdiction: in.DefaultJurisdiction,
			IsTaxInclusive: in.IsTaxInclusive, ContactEmail: in.ContactEmail,
			Hostnames: domain.OrderHostnames(in.Hostnames, in.Hostnames[0]),
			CreatedAt: now, UpdatedAt: now,
		}
		var didCreate bool
		err = s.run(ctx, func(tx pgx.Tx) error {
			if _, ok, err := s.repo.Get(ctx, tx, t.TenantID, false); err != nil {
				return err
			} else if ok {
				return nil
			}
			if err := s.checkHostnamesFree(ctx, tx, t.TenantID, in.Hostnames); err != nil {
				return err
			}
			if err := s.repo.Insert(ctx, tx, t); err != nil {
				return conflict(err)
			}
			didCreate = true
			return nil
		})
		if err != nil {
			return created, skipped, fmt.Errorf("import %s: %w", r.TenantID, err)
		}
		if didCreate {
			created++
		} else {
			skipped++
		}
	}
	return created, skipped, nil
}
