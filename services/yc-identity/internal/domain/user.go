// Package domain holds yc-identity's types. It does no I/O.
package domain

import (
	"net/mail"
	"regexp"
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

	// RoleService is carried only by service tokens (ADR-021); no user
	// has it, and Valid reports false for it.
	RoleService Role = "service"
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
	case RoleService:
		return 0
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
	PhoneE164    string // "" when unset; E.164 when set
	ProfileImage string // "" when unset; https URL from OAuth
	PasswordHash string // never leaves the service
	Role         Role
	Status       Status
	CreatedAt    time.Time
}

// Session is an issued access token and the identity it carries, with a
// refresh token for users who are not visitors.
type Session struct {
	Token     string
	TokenID   string // jti
	UserID    string
	TenantID  string
	Role      Role
	ExpiresAt time.Time

	RefreshToken     string // "" for visitors and service tokens
	RefreshExpiresAt time.Time
}

// OAuthProfile is what a provider returned about the account that signed
// in.
type OAuthProfile struct {
	Subject       string
	Email         string // normalised; "" if the provider gave none
	EmailVerified bool
	Name          string
	PhoneE164     string // normalised E.164; "" if absent or invalid (skipped)
	ProfileImage  string // https URL; "" if absent or invalid (skipped)
}

// MaxProfileImageURL caps OAuth profile image URLs (OpenAPI / storage).
const MaxProfileImageURL = 2048

var e164Re = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// NormalisePhoneE164 trims and validates an E.164 number. Empty in → empty out.
// Invalid non-empty values return ok=false so callers can skip the claim.
func NormalisePhoneE164(phone string) (string, bool) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", true
	}
	if !e164Re.MatchString(phone) {
		return "", false
	}
	return phone, true
}

// NormaliseProfileImageURL accepts https URLs only. Empty in → empty out.
// Invalid non-empty values return ok=false so callers can skip the claim.
func NormaliseProfileImageURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if len(raw) > MaxProfileImageURL || !strings.HasPrefix(raw, "https://") {
		return "", false
	}
	return raw, true
}

// MergeOAuthProfile applies provider phone/image per WP3 rules: set phone
// only when the user has none; always refresh image when the provider sends
// one. Omitted claims do not clear existing values. Invalid claims are ignored.
func (u *User) MergeOAuthProfile(p OAuthProfile) {
	if phone, ok := NormalisePhoneE164(p.PhoneE164); ok && phone != "" && u.PhoneE164 == "" {
		u.PhoneE164 = phone
	}
	if img, ok := NormaliseProfileImageURL(p.ProfileImage); ok && img != "" {
		u.ProfileImage = img
	}
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
	if !validPasswordLen(password) {
		v.Field("password", "invalid_length", "Password must be 10 to 128 characters")
	}
	displayName, ok = NormaliseDisplayName(displayName)
	if !ok {
		v.Field("display_name", "too_long", "Display name must be at most 100 characters")
	}
	if v.HasErrors() {
		return Registration{}, v.Build()
	}
	return Registration{Email: norm, Password: password, DisplayName: displayName}, nil
}

// ValidatePassword checks a new password's length.
func ValidatePassword(password string) error {
	if !validPasswordLen(password) {
		return ycerrors.Validation().Field("password", "invalid_length", "Password must be 10 to 128 characters").Build()
	}
	return nil
}

func validPasswordLen(password string) bool {
	n := utf8.RuneCountInString(password)
	return n >= MinPasswordLen && n <= MaxPasswordLen
}

// NormaliseDisplayName trims a display name and reports whether it is at
// most 100 characters.
func NormaliseDisplayName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, utf8.RuneCountInString(name) <= 100
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
