package domain

import (
	"strings"
	"time"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

// InvitationStatus is an invitation's lifecycle state (ADR-030).
type InvitationStatus string

// Invitation statuses.
const (
	InvitationPending  InvitationStatus = "pending"
	InvitationAccepted InvitationStatus = "accepted"
	InvitationRevoked  InvitationStatus = "revoked"
	InvitationExpired  InvitationStatus = "expired"
)

// Valid reports whether s is a known invitation status.
func (s InvitationStatus) Valid() bool {
	switch s {
	case InvitationPending, InvitationAccepted, InvitationRevoked, InvitationExpired:
		return true
	default:
		return false
	}
}

// Invitation is a row of the invitations table.
type Invitation struct {
	ID             string
	TenantID       string
	Email          string
	Role           Role
	Status         InvitationStatus
	DisplayName    string
	ExpiresAt      time.Time
	InvitedBy      string
	AcceptedUserID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	RevokedAt      *time.Time
}

// EffectiveStatus is Status, or expired when a pending invite has passed
// ExpiresAt.
func (inv Invitation) EffectiveStatus(now time.Time) InvitationStatus {
	if inv.Status == InvitationPending && !now.Before(inv.ExpiresAt) {
		return InvitationExpired
	}
	return inv.Status
}

// InviteInput is a validated create-invitation request (ADR-030 rule 1).
type InviteInput struct {
	TenantID    string
	Email       string
	Role        Role
	DisplayName string
	AcceptHost  string
}

// NewInviteInput normalises and checks tenant/role pairing and email.
func NewInviteInput(tenantID, email, role, displayName, acceptHost string) (InviteInput, error) {
	v := ycerrors.Validation()
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		v.Field("tenant_id", "required", "Tenant is required")
	} else if err := eventbus.ValidateTenantID(tenantID); err != nil {
		v.Field("tenant_id", "invalid", "Tenant ID is not valid")
	}
	norm, ok := NormaliseEmail(email)
	if !ok {
		v.Field("email", "invalid", "Email is not valid")
	}
	r := Role(role)
	switch r {
	case RoleSystemAdmin:
		if tenantID != "" && tenantID != dbkit.PlatformTenantID {
			v.Field("tenant_id", "invalid", "system_admin invitations belong to _platform")
		}
	case RoleShopAdmin:
		if tenantID == dbkit.PlatformTenantID {
			v.Field("tenant_id", "invalid", "shop_admin invitations belong to a shop")
		}
	case RoleVisitor, RoleCustomer, RoleService:
		v.Field("role", "invalid", "Role must be shop_admin or system_admin")
	default:
		v.Field("role", "invalid", "Role must be shop_admin or system_admin")
	}
	displayName, nameOK := NormaliseDisplayName(displayName)
	if !nameOK {
		v.Field("display_name", "too_long", "Display name must be at most 100 characters")
	}
	acceptHost = strings.ToLower(strings.TrimSpace(acceptHost))
	if v.HasErrors() {
		return InviteInput{}, v.Build()
	}
	return InviteInput{
		TenantID: tenantID, Email: norm, Role: r,
		DisplayName: displayName, AcceptHost: acceptHost,
	}, nil
}

// AcceptInviteInput is a validated accept-invitation request.
type AcceptInviteInput struct {
	Token       string
	Password    string
	DisplayName string
}

// NewAcceptInviteInput checks the password policy and optional display name.
func NewAcceptInviteInput(token, password, displayName string) (AcceptInviteInput, error) {
	v := ycerrors.Validation()
	if !validPasswordLen(password) {
		v.Field("password", "invalid_length", "Password must be 10 to 128 characters")
	}
	displayName, ok := NormaliseDisplayName(displayName)
	if !ok {
		v.Field("display_name", "too_long", "Display name must be at most 100 characters")
	}
	if v.HasErrors() {
		return AcceptInviteInput{}, v.Build()
	}
	return AcceptInviteInput{Token: token, Password: password, DisplayName: displayName}, nil
}
