package integration

import "context"

// ClientsDirectory adapts Identity and Tenant clients to service.Directory.
type ClientsDirectory struct {
	Identity *Identity
	Tenant   *Tenant
}

// UserBilling loads saved customer billing.
func (d ClientsDirectory) UserBilling(ctx context.Context, userID string) (BillingAddress, bool, error) {
	if d.Identity == nil {
		return BillingAddress{}, false, nil
	}
	return d.Identity.UserBilling(ctx, userID)
}

// SellerInvoice loads shop invoice identity.
func (d ClientsDirectory) SellerInvoice(ctx context.Context, tenantID string) (SellerInvoice, error) {
	if d.Tenant == nil {
		return SellerInvoice{}, nil
	}
	return d.Tenant.SellerInvoice(ctx, tenantID)
}
