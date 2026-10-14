// Package domain holds yc-identity's types. It does no I/O.
package domain

import (
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// Role is a user's role. It is also the actor type of the user's tokens.
type Role string

// Roles.
const (
	RoleVisitor     Role = "visitor"
	RoleCustomer    Role = "customer"
	RoleShopAdmin   Role = "shop_admin"
	RoleSystemAdmin Role = "system_admin"
)

// ActorType is the actor type for tokens and events.
func (r Role) ActorType() authctx.ActorType { return authctx.ActorType(r) }

// Status is a user's lifecycle state.
type Status string

// Statuses.
const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusDeleted   Status = "deleted"
)

// User is a row of the users table. Visitors are users with RoleVisitor
// and no credentials.
type User struct {
	ID        string
	TenantID  string
	Role      Role
	Status    Status
	CreatedAt time.Time
}

// Session is an issued access token and the identity it carries.
type Session struct {
	Token     string
	TokenID   string // jti
	UserID    string
	TenantID  string
	Role      Role
	ExpiresAt time.Time
}
