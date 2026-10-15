// Package domain holds yc-identity's types. It does no I/O.
package domain

import (
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
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

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return r.rank() > 0
}

// Above reports whether r is higher than o in the linear hierarchy
// visitor < customer < shop_admin < system_admin.
func (r Role) Above(o Role) bool { return r.rank() > o.rank() }

func (r Role) rank() int {
	switch r {
	case RoleVisitor:
		return 1
	case RoleCustomer:
		return 2
	case RoleShopAdmin:
		return 3
	case RoleSystemAdmin:
		return 4
	default:
		return 0
	}
}

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
	ID           string
	TenantID     string
	Email        string // "" for visitors
	DisplayName  string
	PasswordHash string // never leaves the service
	Role         Role
	Status       Status
	CreatedAt    time.Time
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

// Password length limits. The upper bound keeps hashing cost bounded.
const (
	MinPasswordLen = 10
	MaxPasswordLen = 128
)

// Registration is a validated email + password sign-up.
type Registration struct {
	Email       string
	Password    string
	DisplayName string
}

// NewRegistration normalises and validates a sign-up: the email is trimmed
// and lowercased, the password must be 10–128 characters.
func NewRegistration(email, password, displayName string) (Registration, error) {
	v := ycerrors.Validation()
	norm, ok := NormaliseEmail(email)
	if !ok {
		v.Field("email", "invalid", "Email is not valid")
	}
	if n := utf8.RuneCountInString(password); n < MinPasswordLen || n > MaxPasswordLen {
		v.Field("password", "invalid_length", "Password must be 10 to 128 characters")
	}
	displayName = strings.TrimSpace(displayName)
	if utf8.RuneCountInString(displayName) > 100 {
		v.Field("display_name", "too_long", "Display name must be at most 100 characters")
	}
	if v.HasErrors() {
		return Registration{}, v.Build()
	}
	return Registration{Email: norm, Password: password, DisplayName: displayName}, nil
}

// NormaliseEmail trims and lowercases a bare address ("a@b.c", no display
// name) and reports whether it is valid.
func NormaliseEmail(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(email) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", false
	}
	return email, true
}
