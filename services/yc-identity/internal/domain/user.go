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

// MergeOAuthProfile applies provider phone/image: set each only when the
// stored value is empty (issue #37; amends #27 image refresh). Omitted
// claims do not clear existing values. Invalid claims are ignored.
func (u *User) MergeOAuthProfile(p OAuthProfile) {
	if phone, ok := NormalisePhoneE164(p.PhoneE164); ok && phone != "" && u.PhoneE164 == "" {
		u.PhoneE164 = phone
	}
	if img, ok := NormaliseProfileImageURL(p.ProfileImage); ok && img != "" && u.ProfileImage == "" {
		u.ProfileImage = img
	}
}

// FieldPatch is one optional string field in a PATCH body.
// Set=false → leave unchanged; Set && Clear → clear; Set && !Clear → Value.
type FieldPatch struct {
	Set   bool
	Clear bool
	Value string
}

// ProfilePatch is a validated partial update of the caller's profile (#37).
type ProfilePatch struct {
	DisplayName  FieldPatch
	PhoneE164    FieldPatch
	ProfileImage FieldPatch
}

// Apply writes set fields onto u.
func (p ProfilePatch) Apply(u *User) {
	if p.DisplayName.Set {
		if p.DisplayName.Clear {
			u.DisplayName = ""
		} else {
			u.DisplayName = p.DisplayName.Value
		}
	}
	if p.PhoneE164.Set {
		if p.PhoneE164.Clear {
			u.PhoneE164 = ""
		} else {
			u.PhoneE164 = p.PhoneE164.Value
		}
	}
	if p.ProfileImage.Set {
		if p.ProfileImage.Clear {
			u.ProfileImage = ""
		} else {
			u.ProfileImage = p.ProfileImage.Value
		}
	}
}

// NewProfilePatch validates a PATCH body. clearX is true when the JSON
// value was null. presentX is true when the property was sent. Empty string
// clears display_name; for phone and image it is a validation error.
func NewProfilePatch(
	displayPresent bool, displayClear bool, displayValue string,
	phonePresent bool, phoneClear bool, phoneValue string,
	imagePresent bool, imageClear bool, imageValue string,
) (ProfilePatch, error) {
	if !displayPresent && !phonePresent && !imagePresent {
		return ProfilePatch{}, ycerrors.Validation().Message("At least one profile field is required").Build()
	}
	v := ycerrors.Validation()
	var out ProfilePatch

	if displayPresent {
		out.DisplayName.Set = true
		if displayClear || strings.TrimSpace(displayValue) == "" {
			out.DisplayName.Clear = true
		} else {
			name, ok := NormaliseDisplayName(displayValue)
			if !ok {
				v.Field("display_name", "too_long", "Display name must be at most 100 characters")
			} else {
				out.DisplayName.Value = name
			}
		}
	}

	if phonePresent {
		out.PhoneE164.Set = true
		if phoneClear {
			out.PhoneE164.Clear = true
		} else if strings.TrimSpace(phoneValue) == "" {
			v.Field("phone_e164", "invalid", "Use null to clear phone; empty string is not allowed")
		} else {
			phone, ok := NormalisePhoneE164(phoneValue)
			if !ok || phone == "" {
				v.Field("phone_e164", "invalid", "Phone must be E.164")
			} else {
				out.PhoneE164.Value = phone
			}
		}
	}

	if imagePresent {
		out.ProfileImage.Set = true
		if imageClear {
			out.ProfileImage.Clear = true
		} else if strings.TrimSpace(imageValue) == "" {
			v.Field("profile_image_url", "invalid", "Use null to clear avatar; empty string is not allowed")
		} else {
			img, ok := NormaliseProfileImageURL(imageValue)
			if !ok || img == "" {
				v.Field("profile_image_url", "invalid", "Profile image must be an https URL")
			} else {
				out.ProfileImage.Value = img
			}
		}
	}

	if v.HasErrors() {
		return ProfilePatch{}, v.Build()
	}
	return out, nil
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
